package main

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"telegram-gemini-bot/internal/llm"
)

type trayWorkerAI struct {
	calls    int
	response string
	request  llm.Request
	err      error
}

func (a *trayWorkerAI) Generate(_ context.Context, r llm.Request) (llm.Response, error) {
	a.calls++
	a.request = r
	return llm.Response{Text: a.response}, a.err
}

func trayWorkerFixture(t *testing.T) (*trayPhotos, *trayWorkerAI, time.Time) {
	t.Helper()
	s := testStore(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	ai := &trayWorkerAI{}
	p := &trayPhotos{store: s, ai: ai, model: "test", dailyCalls: 20, frames: t.TempDir(), admins: map[int64]bool{1: true}, now: func() time.Time { return now }}
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	img.Set(0, 0, color.White)
	f, err := os.Create(filepath.Join(p.frames, "test.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err = jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = s.db.Exec(`INSERT INTO tray_submissions(id,user_id,service_date,meal,upload_day,digest,frame,dishes,status,created_at) VALUES(1,99,'2026-11-21','LUNCH','2026-11-21','test','test.jpg','[{"id":7,"name":"rice"}]','queued',?)`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	return p, ai, now
}

func TestTrayWorkerProcessesAndValidates(t *testing.T) {
	p, ai, _ := trayWorkerFixture(t)
	ai.response = `{"suggestions":[{"dish_id":7,"x":0,"y":0,"w":500,"h":500,"selected":true,"reason":"","replace":""}]}`
	if err := p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status, suggestions string
	if err := p.store.db.QueryRow(`SELECT status,suggestions FROM tray_submissions WHERE id=1`).Scan(&status, &suggestions); err != nil {
		t.Fatal(err)
	}
	if status != "review" || ai.calls != 1 || len(ai.request.Tools) != 0 || ai.request.ResponseSchema == nil {
		t.Fatalf("status=%s calls=%d request=%+v", status, ai.calls, ai.request)
	}
	if countRows(t, p.store, "tray_notifications") != 1 {
		t.Fatal("missing notification")
	}
	if len(suggestions) < 3 {
		t.Fatalf("empty suggestions: %s", suggestions)
	}
}

func TestTrayWorkerInvalidResponseAndRecovery(t *testing.T) {
	p, ai, _ := trayWorkerFixture(t)
	ai.response = `{"suggestions":[{"dish_id":999,"x":0,"y":0,"w":500,"h":500,"selected":true,"reason":"","replace":""}]}`
	if err := p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := p.store.db.QueryRow(`SELECT reason FROM tray_submissions WHERE id=1`).Scan(&reason); err != nil || reason == "" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	if _, err := p.store.db.Exec(`UPDATE tray_submissions SET status='queued',started_at=123`); err != nil {
		t.Fatal(err)
	}
	if err := p.recoverTrayClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ai.calls != 1 {
		t.Fatal("recovery retried AI")
	}
	var status string
	if err := p.store.db.QueryRow(`SELECT status FROM tray_submissions`).Scan(&status); err != nil || status != "review" {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestTrayWorkerBudgetAndNotifications(t *testing.T) {
	p, ai, now := trayWorkerFixture(t)
	p.dailyCalls = 1
	if _, err := p.store.db.Exec(`INSERT INTO tray_ai_calls VALUES(?,1)`, now.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	if err := p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ai.calls != 0 {
		t.Fatal("budget exceeded")
	}
	calls := 0
	p.notify = func(admin, id int64) error { calls++; return errors.New("temporary") }
	if err := p.processTrayNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.processTrayNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("backoff failed: %d", calls)
	}
	if _, err := p.store.db.Exec(`UPDATE tray_submissions SET status='closed'`); err != nil {
		t.Fatal(err)
	}
	if err := p.processTrayNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countRows(t, p.store, "tray_notifications") != 0 {
		t.Fatal("closed notification retained")
	}
}

func TestTrayWorkerPurgesClosedFrames(t *testing.T) {
	p, _, now := trayWorkerFixture(t)
	if _, err := p.store.db.Exec(`UPDATE tray_submissions SET status='closed',closed_at=?`, now.Add(-trayRetention-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := p.purgeTrayFrames(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.frames, "test.jpg")); !os.IsNotExist(err) {
		t.Fatalf("frame still exists: %v", err)
	}
	var frame string
	if err := p.store.db.QueryRow(`SELECT frame FROM tray_submissions`).Scan(&frame); err != nil || frame != "" {
		t.Fatalf("frame=%q err=%v", frame, err)
	}
}
