package config

import (
	"strings"
	"testing"
)

const twoBoxes = `
supervisor:
  goal_labels: "goal:"
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

func TestCrewWorkersArePeersOfEachOtherAcrossMachines(t *testing.T) {
	cfg, err := parsePeers(t, "crew: true, command: claude --remote-control", "", "crew: true, command: claude --remote-control")
	if err != nil {
		t.Fatal(err)
	}
	app, api, web := cfg.Machines[0].Workers[0], cfg.Machines[0].Workers[1], cfg.Machines[1].Workers[0]
	if len(app.PeerList) != 1 || app.PeerList[0].Session != "mini-web" || !app.PeerList[0].Remote {
		t.Errorf("app's peers = %+v, want mini-web", app.PeerList)
	}
	if len(web.PeerList) != 1 || web.PeerList[0].Session != "box-app" || web.Session != "mini-web" {
		t.Errorf("web = %+v, want box-app as its peer and mini-web as its session", web)
	}
	if len(api.PeerList) != 0 {
		t.Errorf("api is not crew; its peers = %+v", api.PeerList)
	}
}

func TestCrewOnTwoMachinesNeedsRemoteControl(t *testing.T) {
	_, err := parsePeers(t, "crew: true", "", "crew: true")
	if err == nil || !strings.Contains(err.Error(), `are crew on different machines, so both commands need --remote-control`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := parsePeers(t, "crew: true, ask: true", "", ""); err == nil || !strings.Contains(err.Error(), "cannot be crew") {
		t.Fatalf("an ask worker as crew: err = %v", err)
	}
}

func TestCrewNeedsGoalLabels(t *testing.T) {
	_, err := Parse([]byte("machines:\n  - name: box\n    host: local\n    workers:\n      - {name: app, dir: /tmp, brief: b, crew: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "set supervisor.goal_labels") {
		t.Fatalf("err = %v", err)
	}
}
