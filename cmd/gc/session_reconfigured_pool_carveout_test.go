package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// ga-k88yuh reproduction. Shapes are copied from the live fleet beads
// gm-wisp-f7n (gascity/deployer) and gm-wisp-5rkzw (MCDClient/builder): a
// canonical-singleton pool bead (session_name "<rig>--<role>-pool") that the
// named-session desired state adopted via its alias, so it now carries
// configured_named_session=true while its session_name is NOT the name
// config.NamedSessionRuntimeName reserves for the identity.

const (
	k88Identity = "gascity/deployer"
	k88PoolName = "gascity--deployer-pool"
	k88Stale    = "codex-strip --dangerously-skip-permissions --model claude-sonnet-5 --dangerously-bypass-approvals-and-sandbox --model gpt-5.6-sol -c model_reasoning_effort=xhigh"
	k88Fresh    = "codex-strip --dangerously-skip-permissions --model claude-sonnet-5"
)

func k88Config() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "deployer", Dir: "gascity", StartCommand: "true", MaxActiveSessions: intPtr(1)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "deployer", Dir: "gascity", Mode: "on_demand"},
		},
	}
}

func k88SeedSessionBead(t *testing.T, store beads.Store, sessionName string, named bool, extra map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{
		"session_name":   sessionName,
		"alias":          k88Identity,
		"template":       k88Identity,
		"agent_name":     k88Identity,
		"state":          "asleep",
		"session_origin": "named",
		"wake_mode":      "fresh",
		"command":        k88Stale,
		"synced_at":      "2026-09-08T03:44:52Z",
	}
	if named {
		meta[namedSessionMetadataKey] = "true"
		meta[namedSessionIdentityMetadata] = k88Identity
		meta[namedSessionModeMetadata] = "on_demand"
	}
	for k, v := range extra {
		meta[k] = v
	}
	b, err := store.Create(beads.Bead{
		Title:    k88Identity,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	return b
}

func k88SeedAssignedWork(t *testing.T, store beads.Store) {
	t.Helper()
	w, err := store.Create(beads.Bead{Title: "needs-deploy: something", Type: "task", Assignee: k88Identity})
	if err != nil {
		t.Fatalf("seeding work bead: %v", err)
	}
	status := "in_progress"
	if err := store.Update(w.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("claiming work bead: %v", err)
	}
}

// k88NamedDesired is the desired-state entry buildDesiredState produces for the
// named session when it finds the adopted bead as canonical: keyed by the
// canonical bead's OWN session_name (build_desired_state.go "When a canonical
// bead exists, use ITS session_name as the desiredState key").
func k88NamedDesired(sessionName string) map[string]TemplateParams {
	return map[string]TemplateParams{
		sessionName: {
			SessionName:             sessionName,
			TemplateName:            k88Identity,
			InstanceName:            k88Identity,
			Alias:                   k88Identity,
			Command:                 k88Fresh,
			WakeMode:                "fresh",
			ConfiguredNamedIdentity: k88Identity,
			ConfiguredNamedMode:     "on_demand",
		},
	}
}

func k88Sync(t *testing.T, store beads.Store, cfg *config.City, sp runtime.Provider, ds map[string]TemplateParams) string {
	t.Helper()
	clk := &clock.Fake{Time: time.Date(2026, 9, 24, 20, 30, 0, 0, time.UTC)}
	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)
	return stderr.String()
}

// RED on main: ask 1. The per-tick sync never refreshes "command" on an
// adopted -pool named bead that holds assigned work.
func TestK88_SyncRefreshesCommandOnAdoptedPoolNamedBeadWithAssignedWork(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	b := k88SeedSessionBead(t, store, k88PoolName, true, nil)
	k88SeedAssignedWork(t, store)

	if spec, ok := findNamedSessionSpec(cfg, "test-city", k88Identity); !ok || spec.SessionName == k88PoolName {
		t.Fatalf("precondition: spec.SessionName=%q ok=%v, want a name != %q", spec.SessionName, ok, k88PoolName)
	}

	log := k88Sync(t, store, cfg, sp, k88NamedDesired(k88PoolName))

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" {
		t.Fatalf("bead status=%q, want open (it holds assigned work)", got.Status)
	}
	if got.Metadata["command"] != k88Fresh {
		t.Fatalf("sync did not refresh command on adopted -pool named bead:\n  stored  = %q\n  desired = %q\n  synced_at=%q stderr=%q",
			got.Metadata["command"], k88Fresh, got.Metadata["synced_at"], log)
	}
}

// Control (disproves "sync skips named beads with assigned work in general"):
// the SAME bead with session_name == spec.SessionName is refreshed.
func TestK88_Control_SyncRefreshesCommandWhenSessionNameMatchesSpec(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	spec, _ := findNamedSessionSpec(cfg, "test-city", k88Identity)
	b := k88SeedSessionBead(t, store, spec.SessionName, true, nil)
	k88SeedAssignedWork(t, store)

	k88Sync(t, store, cfg, sp, k88NamedDesired(spec.SessionName))

	got, _ := store.Get(b.ID)
	if got.Metadata["command"] != k88Fresh {
		t.Fatalf("control: command=%q, want %q", got.Metadata["command"], k88Fresh)
	}
}

// Control (disproves the bead's "an earlier continue skips pool beads" guess):
// the SAME -pool bead WITHOUT the configured-named stamps, desired as a plain
// canonical-singleton pool entry, is refreshed.
func TestK88_Control_SyncRefreshesCommandOnPlainPoolBead(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	b := k88SeedSessionBead(t, store, k88PoolName, false, map[string]string{"pool_managed": "true", "session_origin": "ephemeral"})
	k88SeedAssignedWork(t, store)

	ds := map[string]TemplateParams{
		k88PoolName: {
			SessionName:  k88PoolName,
			TemplateName: k88Identity,
			InstanceName: k88Identity,
			Alias:        k88Identity,
			Command:      k88Fresh,
			WakeMode:     "fresh",
		},
	}
	k88Sync(t, store, cfg, sp, ds)

	got, _ := store.Get(b.ID)
	if got.Metadata["command"] != k88Fresh {
		t.Fatalf("control: command=%q, want %q", got.Metadata["command"], k88Fresh)
	}
}

// Observation (the other face of the same gate): with NO assigned work and the
// runtime stopped, the adopted bead is not skipped but CLOSED as reconfigured.
func TestK88_Observation_AdoptedPoolNamedBeadWithoutWorkIsClosedAsReconfigured(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	b := k88SeedSessionBead(t, store, k88PoolName, true, nil)

	k88Sync(t, store, cfg, sp, k88NamedDesired(k88PoolName))

	got, _ := store.Get(b.ID)
	t.Logf("no-work adopted bead: status=%q close_reason=%q", got.Status, got.Metadata["close_reason"])
	if got.Status != "closed" || got.Metadata["close_reason"] != session.CanonicalCloseReason("reconfigured") {
		t.Fatalf("status=%q close_reason=%q, want closed/reconfigured", got.Status, got.Metadata["close_reason"])
	}
}

// Artifact: how the fleet beads got this shape. A plain canonical-singleton
// pool bead (alias == named identity) is found canonical for the named spec by
// FindCanonicalNamedSessionInfo's alias pass; the sync tick that follows stamps
// the configured-named metadata onto it. That tick is the LAST sync write: from
// the next tick on the reconfigured gate blocks the identity. Live evidence:
// every adopted -pool named bead has synced_at frozen 24-36 min after creation.
func TestK88_Artifact_AdoptionTickIsLastSyncWrite(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	b := k88SeedSessionBead(t, store, k88PoolName, false, map[string]string{"pool_managed": "true", "session_origin": "ephemeral"})
	k88SeedAssignedWork(t, store)

	spec, _ := findNamedSessionSpec(cfg, "test-city", k88Identity)
	seeded, _ := store.Get(b.ID)
	canon, ok := session.FindCanonicalNamedSessionInfo([]session.Info{sessiontest.SeedBead(t, seeded)}, spec)
	t.Logf("alias pass: canonical=%v id=%s sn=%q (spec.SessionName=%q)", ok, canon.ID, canon.SessionNameMetadata, spec.SessionName)
	if !ok || canon.ID != b.ID {
		t.Fatalf("plain -pool bead was not adopted as canonical: ok=%v id=%q", ok, canon.ID)
	}

	k88Sync(t, store, cfg, sp, k88NamedDesired(k88PoolName)) // adoption tick
	tick1, _ := store.Get(b.ID)
	t.Logf("tick1: named=%q command_fresh=%v synced_at=%q", tick1.Metadata[namedSessionMetadataKey], tick1.Metadata["command"] == k88Fresh, tick1.Metadata["synced_at"])
	if tick1.Metadata[namedSessionMetadataKey] != "true" || tick1.Metadata["command"] != k88Fresh {
		t.Fatalf("adoption tick did not stamp named metadata + command")
	}

	ds := k88NamedDesired(k88PoolName)
	tp := ds[k88PoolName]
	tp.Command = k88Fresh + " --model gpt-6-sol"
	ds[k88PoolName] = tp
	k88Sync(t, store, cfg, sp, ds) // any later config change
	tick2, _ := store.Get(b.ID)
	t.Logf("tick2: command=%q synced_at=%q", tick2.Metadata["command"], tick2.Metadata["synced_at"])
	if tick2.Metadata["command"] != tp.Command {
		t.Fatalf("tick2 did not refresh the command: got %q, want %q (the freeze reproduced)", tick2.Metadata["command"], tp.Command)
	}
}
