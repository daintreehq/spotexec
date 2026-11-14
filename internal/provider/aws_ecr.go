package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// ImageRegistry is implemented by providers that can store built project
// images, so a Dockerfile is built once and other runners pull it.
type ImageRegistry interface {
	RegistryFor(ctx context.Context, repoID string) (*protocol.RegistryAuth, error)
}

// RegistryFor returns a short-lived ECR login for the repository
// spotexec/<repo-id>, creating it (keeping the newest 10 images) on first
// use. The token goes to the runner for one pull/push and is never stored
// in its default docker config.
func (a *AWSSpot) RegistryFor(ctx context.Context, repoID string) (*protocol.RegistryAuth, error) {
	name := "spotexec/" + ecrName(repoID)
	marker := filepath.Join(a.dir, "ecr-"+a.r.Region+"-"+a.r.Profile+"-"+strings.ReplaceAll(name, "/", "_"))
	uri := ""
	// The cached URI saves two API calls per image, but expires: a deleted
	// repository must be recreated, or every push would fail.
	if fi, err := os.Stat(marker); err == nil && time.Since(fi.ModTime()) < time.Hour {
		if b, err := os.ReadFile(marker); err == nil {
			uri = strings.TrimSpace(string(b))
		}
	}
	if uri == "" {
		var desc struct {
			Repositories []struct {
				URI string `json:"repositoryUri"`
			} `json:"repositories"`
		}
		err := a.ecr(ctx, "DescribeRepositories", map[string]any{"repositoryNames": []string{name}}, &desc)
		policy := `{"rules":[{"rulePriority":1,"description":"keep the newest 10 spotexec images","selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":10},"action":{"type":"expire"}}]}`
		switch {
		case err == nil && len(desc.Repositories) > 0:
			uri = desc.Repositories[0].URI
			// (Re)apply the expiry rule: a first run that created the
			// repository may have failed before setting it.
			if err := a.ecr(ctx, "PutLifecyclePolicy", map[string]any{"repositoryName": name, "lifecyclePolicyText": policy}, nil); err != nil {
				return nil, fmt.Errorf("ECR lifecycle policy for %s: %w", name, err)
			}
		case err == nil || strings.Contains(err.Error(), "RepositoryNotFoundException"):
			var created struct {
				Repository struct {
					URI string `json:"repositoryUri"`
				} `json:"repository"`
			}
			if err := a.ecr(ctx, "CreateRepository", map[string]any{
				"repositoryName": name, "imageTagMutability": "MUTABLE",
				"tags": []map[string]string{{"Key": tagRunner, "Value": a.r.Name}},
			}, &created); err != nil && !strings.Contains(err.Error(), "RepositoryAlreadyExistsException") {
				return nil, fmt.Errorf("creating ECR repository %s: %w", name, err)
			}
			uri = created.Repository.URI
			if err := a.ecr(ctx, "PutLifecyclePolicy", map[string]any{"repositoryName": name, "lifecyclePolicyText": policy}, nil); err != nil {
				return nil, fmt.Errorf("ECR lifecycle policy for %s: %w", name, err)
			}
			if uri == "" {
				return a.RegistryFor(ctx, repoID) // created concurrently: describe again
			}
		default:
			return nil, fmt.Errorf("ECR: %w", err)
		}
		_ = os.MkdirAll(a.dir, 0o700)
		_ = os.WriteFile(marker, []byte(uri+"\n"), 0o600)
	}
	var tok struct {
		Data []struct {
			Token    string `json:"authorizationToken"`
			Endpoint string `json:"proxyEndpoint"`
		} `json:"authorizationData"`
	}
	if err := a.ecr(ctx, "GetAuthorizationToken", map[string]any{}, &tok); err != nil {
		return nil, fmt.Errorf("ECR login: %w", err)
	}
	if len(tok.Data) == 0 {
		return nil, fmt.Errorf("ECR login: no authorization data")
	}
	raw, err := base64.StdEncoding.DecodeString(tok.Data[0].Token)
	if err != nil {
		return nil, err
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return nil, fmt.Errorf("ECR login: malformed token")
	}
	server := strings.TrimPrefix(tok.Data[0].Endpoint, "https://")
	if !strings.HasPrefix(uri, server+"/") {
		_ = os.Remove(marker) // stale record (other account/region): re-describe next time
		return nil, fmt.Errorf("ECR repository %s is not in registry %s", uri, server)
	}
	return &protocol.RegistryAuth{Server: server, Repo: uri, User: user, Password: pass}, nil
}

// ecr calls an ECR JSON 1.1 action.
func (a *AWSSpot) ecr(ctx context.Context, action string, in any, out any) error {
	body, _ := json.Marshal(in)
	status, b, err := a.api.do(ctx, "ecr", "application/x-amz-json-1.1",
		map[string]string{"X-Amz-Target": "AmazonEC2ContainerRegistry_V20150921." + action}, body)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("%s: %s %s", action, e.Type, e.Message)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// ecrName makes a repo id a valid ECR repository component.
func ecrName(repoID string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(repoID) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
