package app_test

import (
	"testing"
	"time"

	"jarlore/internal/app"
	"jarlore/internal/auth"
)

func TestValidTimezone(t *testing.T) {
	for tz, want := range map[string]bool{
		"Asia/Jakarta": true, "Europe/London": true, "America/New_York": true, "Australia/Sydney": true, "UTC": true,
		"": false, "Local": false, "local": false, "Mars/Olympus_Mons": false, "Asia/Jakarta ": false,
		"../../etc/passwd": false, "+07:00": false,
	} {
		if got := app.ValidTimezone(tz); got != want {
			t.Errorf("ValidTimezone(%q) = %v, want %v", tz, got, want)
		}
	}
}

// The profile starts in the browser's time zone, can be changed later, and the
// zone only changes how times are presented and when "09:00" falls: stored
// timestamps are UTC and never rewritten.
func TestProfileTimezone(t *testing.T) {
	svc, clk, ctx, d := setup(t)
	carol := auth.User{ID: "33333333-3333-4333-8333-333333333333"}
	dan := auth.User{ID: "44444444-4444-4444-8444-444444444444"}
	erin := auth.User{ID: "55555555-5555-4555-8555-555555555555"}
	for u, hint := range map[auth.User]string{carol: "Asia/Jakarta", dan: "Mars/Olympus_Mons", erin: "America/New_York"} {
		if err := svc.EnsureProfile(ctx, u, hint); err != nil {
			t.Fatal(err)
		}
	}
	tzOf := func(s *app.Service, u auth.User) string {
		t.Helper()
		me, err := s.Me(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		return me.Timezone
	}
	if got := tzOf(svc, carol); got != "Asia/Jakarta" {
		t.Errorf("carol created in %q, want Asia/Jakarta", got)
	}
	if got := tzOf(svc, dan); got != "UTC" {
		t.Errorf("an invalid hint must fall back to UTC, got %q", got)
	}
	// A hint never overrides an existing profile (fresh service: no ensure cache).
	fresh := app.New(d.Store, app.Options{CursorSecret: []byte("test-secret"), Now: clk.now})
	if err := fresh.EnsureProfile(ctx, carol, "Europe/London"); err != nil {
		t.Fatal(err)
	}
	if got := tzOf(fresh, carol); got != "Asia/Jakarta" {
		t.Errorf("hint overwrote the profile: %q", got)
	}

	// 09:00 in the user's zone. Carol (WIB, UTC+7) starts at 2026-11-01 08:00Z with a
	// 9-day target: due 2026-11-10 15:00 WIB, so the taste check is 09:00 WIB = 02:00Z.
	draft := kombuchaDraft()
	batch := decode[app.Batch](t)(svc.CreateBatch(ctx, carol, "tz-carol-1", draft))
	reminderAt := func(u auth.User, batchID string) time.Time {
		t.Helper()
		page, err := svc.ListReminders(ctx, u, app.ReminderQuery{BatchID: &batchID})
		if err != nil || len(page.Data) == 0 {
			t.Fatalf("reminders: %v %+v", err, page)
		}
		return page.Data[0].DueAt.UTC()
	}
	if got, want := reminderAt(carol, batch.ID), time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Jakarta taste check at %s, want %s", got, want)
	}

	// Daylight saving: New York leaves EDT at 2026-11-01 06:00Z. A batch started
	// 2026-10-29 12:00Z with a 3-day target is due 2026-11-01 07:00 EST, so the
	// taste check is 09:00 EST = 14:00Z (13:00Z would be the EDT mistake).
	nyDraft := kombuchaDraft()
	nyDraft.Params.TargetF1Days = ptr(3.0)
	started := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	nyDraft.StartedAt = &started
	nyBatch := decode[app.Batch](t)(svc.CreateBatch(ctx, erin, "tz-erin-1", nyDraft))
	if got, want := reminderAt(erin, nyBatch.ID), time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("New York taste check across DST at %s, want %s", got, want)
	}

	// Changing the zone keeps every stored instant.
	before := reminderAt(carol, batch.ID)
	startedBefore := batch.StartedAt
	if _, err := svc.UpdateMe(ctx, carol, app.UpdateMeInput{Timezone: ptr("Europe/London")}); err != nil {
		t.Fatal(err)
	}
	if got := tzOf(svc, carol); got != "Europe/London" {
		t.Errorf("timezone after PATCH = %q", got)
	}
	after, err := svc.GetBatch(ctx, carol, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reminderAt(carol, batch.ID).Equal(before) || !after.StartedAt.Equal(startedBefore) {
		t.Error("changing the time zone rewrote stored timestamps")
	}

	// Invalid zones are refused with a field error.
	for _, bad := range []string{"Asia/Atlantis", "Local", ""} {
		_, err := svc.UpdateMe(ctx, carol, app.UpdateMeInput{Timezone: ptr(bad)})
		wantCode(t, err, "validation_failed")
	}
	clk.add(time.Hour)
}
