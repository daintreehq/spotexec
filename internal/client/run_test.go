package client

import (
	"testing"

	"github.com/spotexec/spotexec/internal/fsid"

	"github.com/spotexec/spotexec/internal/manifest"
	"github.com/spotexec/spotexec/internal/protocol"
)

func mf(entries ...protocol.Entry) *manifest.Manifest {
	m := &manifest.Manifest{ByPath: map[string]protocol.Entry{}}
	for _, e := range entries {
		m.Entries = append(m.Entries, e)
		m.ByPath[e.Path] = e
	}
	return m
}

func TestImageTagScopesAndPlatform(t *testing.T) {
	base := []protocol.Entry{
		{Path: ".spotexec/Dockerfile", ID: "df1", Mode: 0o100644},
		{Path: ".spotexec/setup.sh", ID: "s1", Mode: 0o100755},
		{Path: "src/app.ts", ID: "a1", Mode: 0o100644},
	}
	t1, err := imageTag(mf(base...), ".spotexec/Dockerfile", "", "linux/amd64", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A source change outside the build context must not change the tag.
	changed := append([]protocol.Entry{}, base...)
	changed[2].ID = "a2"
	if t2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", "", "linux/amd64", nil); t2 != t1 {
		t.Fatal("tag changed for a file outside the context")
	}
	// A file inside the context must.
	changed = append([]protocol.Entry{}, base...)
	changed[1].ID = "s2"
	if t2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", "", "linux/amd64", nil); t2 == t1 {
		t.Fatal("tag unchanged for a context file change")
	}
	// Architectures never share a tag.
	if t3, _ := imageTag(mf(base...), ".spotexec/Dockerfile", "", "linux/arm64", nil); t3 == t1 {
		t.Fatal("amd64 and arm64 share a tag")
	}
	// Explicit repo-root context includes everything.
	r1, _ := imageTag(mf(base...), ".spotexec/Dockerfile", ".", "linux/amd64", nil)
	changed = append([]protocol.Entry{}, base...)
	changed[2].ID = "a2"
	if r2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", ".", "linux/amd64", nil); r2 == r1 {
		t.Fatal("repo-root context should include src/")
	}
}

func TestImageTagHonoursDockerignore(t *testing.T) {
	base := []protocol.Entry{
		{Path: "Dockerfile", ID: "df1", Mode: 0o100644},
		{Path: ".dockerignore", ID: "ig1", Mode: 0o100644},
		{Path: "package.json", ID: "p1", Mode: 0o100644},
		{Path: "src/app.ts", ID: "a1", Mode: 0o100644},
	}
	rules := map[string][]byte{".dockerignore": []byte("*\n!package.json\n!Dockerfile\n")}
	read := func(rel string) ([]byte, error) { return rules[rel], nil }
	base[1].ID = fsid.Bytes(rules[".dockerignore"])
	t1, err := imageTag(mf(base...), "Dockerfile", ".", "linux/amd64", read)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]protocol.Entry{}, base...)
	changed[3].ID = "a2"
	if t2, _ := imageTag(mf(changed...), "Dockerfile", ".", "linux/amd64", read); t2 != t1 {
		t.Fatal("an ignored file changed the tag")
	}
	changed = append([]protocol.Entry{}, base...)
	changed[2].ID = "p2"
	if t2, _ := imageTag(mf(changed...), "Dockerfile", ".", "linux/amd64", read); t2 == t1 {
		t.Fatal("a kept file did not change the tag")
	}
	// A Dockerfile-specific ignore file wins over the context's.
	rules["Dockerfile.dockerignore"] = []byte("src\n")
	base = append(base, protocol.Entry{Path: "Dockerfile.dockerignore", ID: fsid.Bytes(rules["Dockerfile.dockerignore"]), Mode: 0o100644})
	t3, _ := imageTag(mf(base...), "Dockerfile", ".", "linux/amd64", read)
	changed = append([]protocol.Entry{}, base...)
	changed[3].ID = "a2"
	if t4, _ := imageTag(mf(changed...), "Dockerfile", ".", "linux/amd64", read); t4 != t3 {
		t.Fatal("Dockerfile.dockerignore not preferred")
	}
}

func TestImageTagRejectsUnsyncedOrSymlinkedRules(t *testing.T) {
	ents := []protocol.Entry{
		{Path: "Dockerfile", ID: "df1", Mode: 0o100644},
		{Path: ".dockerignore", ID: fsid.Bytes([]byte("src\n")), Mode: 0o100644},
	}
	edited := func(string) ([]byte, error) { return []byte("src\npayload\n"), nil }
	if _, err := imageTag(mf(ents...), "Dockerfile", ".", "linux/amd64", edited); err == nil {
		t.Fatal("rules edited after sync were accepted")
	}
	ents[1].Mode = fsid.ModeSymlink
	same := func(string) ([]byte, error) { return []byte("src\n"), nil }
	if _, err := imageTag(mf(ents...), "Dockerfile", ".", "linux/amd64", same); err == nil {
		t.Fatal("symlinked rules were accepted")
	}
}
