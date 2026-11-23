package config

import (
	"strings"
	"testing"
)

const twoBoxes = `
machines:
  - name: box
    host: local
    workers:
      - {name: app, dir: /tmp, brief: b, %s}
      - {name: api, dir: /tmp, brief: b, %s}
  - name: mini
    host: me@mini
    workers:
      - {name: web, dir: /tmp, brief: b, %s}
`

func parsePeers(t *testing.T, app, api, web string) (*Config, error) {
	t.Helper()
	return Parse([]byte(strings.ReplaceAll(strings.Replace(strings.Replace(strings.Replace(twoBoxes, "%s", app, 1), "%s", api, 1), "%s", web, 1), ", }", "}")))
}

func TestPeersAreMutualAndNamedByMachineAndWorker(t *testing.T) {
	cfg, err := parsePeers(t, "peers: [api]", "", "")
	if err != nil {
		t.Fatal(err)
	}
	app, api := cfg.Machines[0].Workers[0], cfg.Machines[0].Workers[1]
	if len(app.PeerList) != 1 || app.PeerList[0].Session != "box-api" || app.PeerList[0].Remote {
		t.Errorf("app's peers = %+v, want box-api on this machine", app.PeerList)
	}
	if len(api.PeerList) != 1 || api.PeerList[0].Session != "box-app" {
		t.Errorf("api's peers = %+v, want box-app: naming a peer makes it mutual", api.PeerList)
	}
	if app.Session != "box-app" {
		t.Errorf("app's session = %q, want box-app", app.Session)
	}
	if web := cfg.Machines[1].Workers[0]; len(web.PeerList) != 0 {
		t.Errorf("web's peers = %+v, want none", web.PeerList)
	}
}

func TestAPeerOnAnotherMachineIsNamedWithItsMachine(t *testing.T) {
	cfg, err := parsePeers(t, "command: claude --remote-control, peers: [mini/web]", "", "command: claude --remote-control")
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Machines[0].Workers[0].PeerList; len(p) != 1 || p[0].Session != "mini-web" || !p[0].Remote {
		t.Errorf("app's peers = %+v, want mini-web on another machine", p)
	}
}

func TestInvalidPeersAreRejected(t *testing.T) {
	for _, c := range []struct{ app, api, web, want string }{
		{"peers: [nope]", "", "", `worker "app": peer "nope" is not a worker of machine "box"`},
		{"peers: [mars/web]", "", "", `worker "app": peer "mars/web" names no machine "mars"`},
		{"peers: [app]", "", "", `worker "app": peer "app" is the worker itself`},
		{"peers: [mini/web]", "", "", `worker "app": peer "mini/web" is on another machine, so both workers' commands need --remote-control`},
		{"peers: [api], command: claude --name mine", "", "", `worker "app" has peers and sets its own session name`},
		{"peers: [api], ask: true", "", "", `worker "app" answers questions and has no peers`},
	} {
		_, err := parsePeers(t, c.app, c.api, c.web)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.app, err, c.want)
		}
	}
}
