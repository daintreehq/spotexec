package cli

import "testing"

func TestRewriteRoot(t *testing.T) {
	root := "/Users/me/repo"
	cases := []struct {
		in      string
		snippet bool
		want    string
	}{
		{"cd /Users/me/repo && npm test", true, "cd ${SPOTEXEC_WORKSPACE:?} && npm test"},
		{"cd /Users/me/repo/pkg && vitest run /Users/me/repo/pkg/a.test.ts", true, "cd ${SPOTEXEC_WORKSPACE:?}/pkg && vitest run ${SPOTEXEC_WORKSPACE:?}/pkg/a.test.ts"},
		{"cat '/Users/me/repo/x'", true, "cat '../x'"},
		{"ls /Users/me/repo-other/x", true, "ls /Users/me/repo-other/x"},
		{"/Users/me/repo/a.txt", false, "../a.txt"},
		{"--out=/Users/me/repo/dist", true, "--out=${SPOTEXEC_WORKSPACE:?}/dist"},
	}
	for _, c := range cases {
		if got := rewriteRoot(c.in, root, "..", c.snippet); got != c.want {
			t.Errorf("rewriteRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
