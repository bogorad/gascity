package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// newIdleOnDemandSeatEnv models the production seat measured on 2026-09-23
// (deep-investigator): a named on_demand session, wake_mode=fresh,
// idle_timeout=2h, alive but idle for longer than the city's 30m
// progress_stall_timeout, holding no claim, with NO demand of any kind —
// no assigned work, no work-query hit, and a zero scale check.
func newIdleOnDemandSeatEnv(t *testing.T) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Agents[0].IdleTimeout = "2h"
	env.cfg.Agents[0].WakeMode = "fresh"
	return env, session, sessionName
}

// noDemand is the scale-check map production computes for the seat
// (pool_desired.compute: reason no_demand, scale_check 0). The harness's
// default reconcile() seeds poolDesired[template]=1, which is itself a wake
// cause, so these tests pass an empty map explicitly on every tick.
func noDemand() map[string]int { return map[string]int{} }

// An idle on_demand named seat with no demand is idle by design: the
// on-demand:running override keeps it until its own idle_timeout drains it.
// The claim-less progress-stall recycle must leave it alone. Recycling it is
// not a no-op: RestartRequestPatch stamps reset_committed_at, and the next
// tick's reset-pending arm (compute_awake_set.go) wakes the seat with no
// demand — which idles past the stall timeout again and is recycled again,
// for as long as no work arrives (measured: ~13 fresh Opus wakes/day on
// deep-investigator).
func TestReconcileSessionBeads_ProgressStallSkipsIdleOnDemandSeatWithoutDemand(t *testing.T) {
	env, session, sessionName := newIdleOnDemandSeatEnv(t)

	for tick := 1; tick <= 2; tick++ {
		current, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("tick %d: store.Get(%s): %v", tick, session.ID, err)
		}
		env.stdout.Reset()
		env.stderr.Reset()
		env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{current}, noDemand(), nil)

		if strings.Contains(env.stderr.String(), "progress-stalled") {
			t.Fatalf("tick %d: stderr = %q, want no progress-stalled recycle for an idle on_demand seat with no demand", tick, env.stderr.String())
		}
		if !env.sp.IsRunning(sessionName) {
			t.Fatalf("tick %d: seat %q stopped; want it left running until its own idle_timeout", tick, sessionName)
		}
		after, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("tick %d: store.Get(%s): %v", tick, session.ID, err)
		}
		if got := after.Metadata[sessionpkg.ResetCommittedAtKey]; got != "" {
			t.Fatalf("tick %d: reset_committed_at = %q, want empty (a committed reset re-wakes the seat without demand)", tick, got)
		}
		env.clk.Time = env.clk.Time.Add(time.Minute)
	}
}

// Control A: with the recycle disabled, the same idle seat is left alone —
// kept by the on-demand:running override until its own idle_timeout drains
// it. No reset is ever committed, so nothing can re-wake it. The loop needs
// the recycle.
func TestReconcileSessionBeads_IdleOnDemandSeatWithoutRecycleCommitsNoReset(t *testing.T) {
	env, session, sessionName := newIdleOnDemandSeatEnv(t)
	env.cfg.Session.ProgressStallTimeout = ""

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, noDemand(), nil)

	if strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want no progress-stalled recycle when the timeout is disabled", env.stderr.String())
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q stopped; want it kept running (on-demand:running) until idle_timeout", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata[sessionpkg.ResetCommittedAtKey] != "" {
		t.Fatalf("reset_committed_at = %q, want empty with the recycle disabled", got.Metadata[sessionpkg.ResetCommittedAtKey])
	}
}

// Control B (attribution): identical post-recycle bead, but without
// reset_committed_at — the shape CompleteDrainPatch(freshWake=true) leaves
// (continuation_reset_pending=true, no commit stamp). The seat stays down. So
// the re-wake comes from the reset_committed_at-gated reset-pending arm
// (compute_awake_bridge.go ContinuationResetPending -> compute_awake_set.go
// desired="reset-pending"), not from the pending flag.
//
// It also disproves a second hypothesis raised by the production bead, which
// carried a stale currently_processing_bead_id pointing at a CLOSED bead: that
// pointer is set here too, and it does not wake the seat.
func TestReconcileSessionBeads_RecycledSeatWithoutResetCommitStaysAsleep(t *testing.T) {
	env, session, sessionName := newIdleOnDemandSeatEnv(t)
	// Force the recycle for this control regardless of any exemption, by
	// seeding the restart request the recycle would have set.
	env.setSessionMetadata(&session, map[string]string{"restart_requested": "true"})

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, noDemand(), nil)
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("precondition: session %q still running after the restart handoff", sessionName)
	}

	closedWork, err := env.store.Create(beads.Bead{Title: "finished investigation", Type: "task", Assignee: sessionName})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := env.store.Close(closedWork.ID); err != nil {
		t.Fatalf("close work bead: %v", err)
	}
	if err := env.store.SetMetadata(session.ID, sessionpkg.ResetCommittedAtKey, ""); err != nil {
		t.Fatalf("clear reset_committed_at: %v", err)
	}
	if err := env.store.SetMetadata(session.ID, "currently_processing_bead_id", closedWork.ID); err != nil {
		t.Fatalf("set currently_processing_bead_id: %v", err)
	}
	drained, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if drained.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("precondition: continuation_reset_pending = %q, want true", drained.Metadata["continuation_reset_pending"])
	}

	env.clk.Time = env.clk.Time.Add(time.Minute)
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{drained}, noDemand(), nil)

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("seat %q woke with continuation_reset_pending=true but no reset_committed_at and no demand", sessionName)
	}
}
