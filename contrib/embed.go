// Package contrib holds the files shed hands out: the service files that run
// the agent on a machine, and the templates it prints for a repository.
package contrib

import _ "embed"

//go:embed shed-agent.service
var SystemdUnit []byte

//go:embed com.edisoncode.shed-agent.plist
var LaunchdPlist []byte

// WorkerIssue is a GitHub issue template for an issue that a worker can take
// with no other context.
//
//go:embed worker-issue.md
var WorkerIssue []byte
