package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/sources"
)

type fakeRunner struct {
	out []byte
	err error
}

func (f fakeRunner) Run(context.Context, config.Machine, string, io.Reader) ([]byte, error) {
	return f.out, f.err
}

func TestARemoteAuditWithFindingsIsReadNotDiscarded(t *testing.T) {
	report, _ := json.Marshal(contextReport{Version: version, Audit: sources.Audit{Total: 1200, Stale: []sources.Stale{{Ref: "internal/gone.go"}}}})
	// A machine on an older shed exits 1 when it finds something.
	audit, err := remoteAudit(context.Background(), fakeRunner{out: report, err: errors.New("box: exit status 1: ")}, config.Machine{Name: "box"}, config.Worker{Name: "app"}, false)
	if err != nil || audit.Total != 1200 || len(audit.Stale) != 1 {
		t.Fatalf("audit = %+v, err = %v", audit, err)
	}
}

func TestARemoteAuditThatPrintedNoReportIsAnError(t *testing.T) {
	_, err := remoteAudit(context.Background(), fakeRunner{err: errors.New("box: exit status 255: Connection refused")}, config.Machine{Name: "box"}, config.Worker{Name: "app"}, false)
	if err == nil {
		t.Fatal("a failed SSH call read as an empty audit")
	}
}
