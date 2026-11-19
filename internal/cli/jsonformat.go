package cli

// The format versions of the commands' JSON output, each carried as a
// top-level format_version (NDJSON events carry ui.EventsFormat). Each
// shape is versioned on its own: within a version, fields are only added;
// renaming, removing or retyping one bumps that shape's version. Human
// output carries no such promise.
const (
	statusFormat        = 1 // status --json
	accountStatusFormat = 1 // status -A --json
	daemonStatusFormat  = 1 // daemon status --json
	pricesFormat        = 1 // prices --json
	syncPlanFormat      = 1 // sync-plan --json
	backupsFormat       = 1 // backups --json
	regionsFormat       = 1 // container regions --json
	discoverFormat      = 1 // hook discover --json
)
