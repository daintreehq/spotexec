package client

import (
	"testing"

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
	t1, err := imageTag(mf(base...), ".spotexec/Dockerfile", "", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	// A source change outside the build context must not change the tag.
	changed := append([]protocol.Entry{}, base...)
	changed[2].ID = "a2"
	if t2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", "", "linux/amd64"); t2 != t1 {
		t.Fatal("tag changed for a file outside the context")
	}
	// A file inside the context must.
	changed = append([]protocol.Entry{}, base...)
	changed[1].ID = "s2"
	if t2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", "", "linux/amd64"); t2 == t1 {
		t.Fatal("tag unchanged for a context file change")
	}
	// Architectures never share a tag.
	if t3, _ := imageTag(mf(base...), ".spotexec/Dockerfile", "", "linux/arm64"); t3 == t1 {
		t.Fatal("amd64 and arm64 share a tag")
	}
	// Explicit repo-root context includes everything.
	r1, _ := imageTag(mf(base...), ".spotexec/Dockerfile", ".", "linux/amd64")
	changed = append([]protocol.Entry{}, base...)
	changed[2].ID = "a2"
	if r2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", ".", "linux/amd64"); r2 == r1 {
		t.Fatal("repo-root context should include src/")
	}
}
