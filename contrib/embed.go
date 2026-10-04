// Package contrib holds the service files that run the agent, so that shed
// can install them on a machine.
package contrib

import _ "embed"

//go:embed shed-agent.service
var SystemdUnit []byte

//go:embed com.edisoncode.shed-agent.plist
var LaunchdPlist []byte
