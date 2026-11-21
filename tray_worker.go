package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"telegram-gemini-bot/internal/llm"
)

const (
	trayWorkerInterval = 5 * time.Second
	trayAITimeout      = 90 * time.Second
	// Orphans are not removed: without a shared upload lock, doing so could
	// race an upload.
)

type trayWorkerJob struct {
	id            int64
	frame, dishes string
}

func (p *trayPhotos) workerNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// run requires exactly one worker instance for this database. started_at is a
// durable one-way claim, not a lease: an interrupted call must never be retried.
func (p *trayPhotos) run(ctx context.Context) {
	if err := p.purgeOrphanTrayFrames(ctx); err != nil {
		log.Printf("tray orphan cleanup: %v", err)
	}
	if err := p.recoverTrayClaims(ctx); err != nil {
		log.Printf("tray recovery: %v", err)
		return
	}
	ticker := time.NewTicker(trayWorkerInterval)
	defer ticker.Stop()
	for {
		if err := p.trayWorkerPass(ctx); err != nil && ctx.Err() == nil {
			log.Printf("tray worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Match only this application's generated basenames. Each lookup and removal
// is serialized with upload's file write and commit on the SQLite connection.
func (p *trayPhotos) purgeOrphanTrayFrames(ctx context.Context) error {
	entries, err := os.ReadDir(p.frames)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || len(name) != 36 || !strings.HasSuffix(name, ".jpg") {
			continue
		}
		if _, err := hex.DecodeString(name[:32]); err != nil {
			continue
		}
		tx, err := p.store.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tray_submissions WHERE frame=?)`, name).Scan(&exists)
		if err == nil && !exists {
			err = os.Remove(filepath.Join(p.frames, name))
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		_ = tx.Rollback()
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *trayPhotos) recoverTrayClaims(ctx context.Context) error {
	rows, err := p.store.db.QueryContext(ctx, `SELECT id FROM tray_submissions WHERE status='queued' AND started_at<>0`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := p.finishTray(ctx, id, nil, "AI processing interrupted; manual review required"); err != nil {
			return err
		}
	}
	return nil
}

func (p *trayPhotos) trayWorkerPass(ctx context.Context) error {
	if err := p.processTrayNotifications(ctx); err != nil {
		return err
	}
	if err := p.purgeTrayFrames(ctx); err != nil {
		return err
	}
	job, err := p.claimTray(ctx)
	if err != nil || job == nil {
		return err
	}
	suggestions, reason := p.suggestTray(ctx, *job)
	// A cancelled process leaves its durable claim for startup recovery.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return p.finishTray(ctx, job.id, suggestions, reason)
}

func (p *trayPhotos) claimTray(ctx context.Context) (*trayWorkerJob, error) {
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var j trayWorkerJob
	err = tx.QueryRowContext(ctx, `SELECT id,frame,dishes FROM tray_submissions WHERE status='queued' AND started_at=0 ORDER BY id LIMIT 1`).Scan(&j.id, &j.frame, &j.dishes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	stamp := p.workerNow().Unix()
	if stamp == 0 {
		stamp = 1
	}
	res, err := tx.ExecContext(ctx, `UPDATE tray_submissions SET started_at=? WHERE id=? AND status='queued' AND started_at=0`, stamp, j.id)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, nil
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &j, nil
}

func (p *trayPhotos) reserveTrayCall(ctx context.Context) (bool, error) {
	limit := p.dailyCalls
	if limit <= 0 {
		limit = 20
	}
	day := p.workerNow().In(seoulLocation).Format("2006-01-02")
	// One atomic statement works even if other processes reserve concurrently.
	result, err := p.store.db.ExecContext(ctx, `INSERT INTO tray_ai_calls(day,calls) VALUES(?,1) ON CONFLICT(day) DO UPDATE SET calls=calls+1 WHERE calls<?`, day, limit)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func trayFramePath(root, name string) (string, error) {
	if name == "" || name == "." || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return "", errors.New("invalid frame name")
	}
	return filepath.Join(root, name), nil
}

func (p *trayPhotos) suggestTray(ctx context.Context, j trayWorkerJob) ([]trayCrop, string) {
	var dishes []trayDish
	if json.Unmarshal([]byte(j.dishes), &dishes) != nil || len(dishes) == 0 {
		return nil, "Invalid dish list; manual review required"
	}
	path, err := trayFramePath(p.frames, j.frame)
	if err != nil {
		return nil, "Invalid frame; manual review required"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "Frame unavailable; manual review required"
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return nil, "Invalid image; manual review required"
	}
	var b bytes.Buffer
	if err = jpeg.Encode(&b, fitTrayImage(img, 1024), &jpeg.Options{Quality: 85}); err != nil {
		return nil, "Image conversion failed; manual review required"
	}
	if p.ai == nil || strings.TrimSpace(p.model) == "" {
		return nil, "AI unavailable; manual review required"
	}
	ok, err := p.reserveTrayCall(ctx)
	if err != nil {
		return nil, "AI budget unavailable; manual review required"
	}
	if !ok {
		return nil, "Daily AI budget exhausted; manual review required"
	}
	item := &llm.Schema{Type: "object", Properties: map[string]*llm.Schema{
		"dish_id": {Type: "integer"}, "x": {Type: "integer"}, "y": {Type: "integer"}, "w": {Type: "integer"}, "h": {Type: "integer"}, "selected": {Type: "boolean"}, "reason": {Type: "string"}, "replace": {Type: "string"},
	}, Required: []string{"dish_id", "x", "y", "w", "h", "selected", "reason", "replace"}}
	schema := &llm.Schema{Type: "object", Properties: map[string]*llm.Schema{"suggestions": {Type: "array", Items: item}}, Required: []string{"suggestions"}}
	prompt := "Identify dishes in this tray photo from the supplied list. Return suggestions with dish_id and bounding boxes x,y,w,h in normalized 0-1000 coordinates. Set selected=false and give a short reason in Russian when uncertain. Use dish_id=0 for an unmatched crop. Never invent dish IDs. Dishes: " + j.dishes
	callCtx, cancel := context.WithTimeout(ctx, trayAITimeout)
	defer cancel()
	resp, err := p.ai.Generate(callCtx, llm.Request{Model: p.model, MaxTokens: 2048, ResponseSchema: schema, Contents: []llm.Content{{Role: "user", Parts: []llm.Part{{Text: prompt}, {Data: b.Bytes(), MimeType: "image/jpeg"}}}}})
	if err != nil {
		return nil, "AI request failed; manual review required"
	}
	var output struct {
		Suggestions []struct {
			DishID   int64  `json:"dish_id"`
			X        int    `json:"x"`
			Y        int    `json:"y"`
			W        int    `json:"w"`
			H        int    `json:"h"`
			Selected bool   `json:"selected"`
			Reason   string `json:"reason"`
			Replace  string `json:"replace"`
		} `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(resp.Text), &output); err != nil || output.Suggestions == nil || len(output.Suggestions) > 32 {
		return nil, "Invalid AI response; manual review required"
	}
	allowed := make(map[int64]bool, len(dishes))
	for _, d := range dishes {
		allowed[d.ID] = true
	}
	selected := map[int64]bool{}
	crops := make([]trayCrop, 0, len(output.Suggestions))
	for _, v := range output.Suggestions {
		reason := []rune(strings.TrimSpace(v.Reason))
		c := trayCrop{DishID: v.DishID, X: v.X, Y: v.Y, W: v.W, H: v.H, Selected: v.Selected, Reason: string(reason[:min(len(reason), 200)])}
		if (!allowed[c.DishID] && (c.DishID != 0 || c.Selected)) || !validTrayCrop(c) || (!c.Selected && c.Reason == "") || (c.Selected && selected[c.DishID]) {
			return nil, "Invalid AI suggestions; manual review required"
		}
		if c.Selected {
			selected[c.DishID] = true
		}
		crops = append(crops, c)
	}
	return crops, ""
}

func (p *trayPhotos) finishTray(ctx context.Context, id int64, crops []trayCrop, reason string) error {
	if crops == nil {
		crops = []trayCrop{}
	}
	data, err := json.Marshal(crops)
	if err != nil {
		return err
	}
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE tray_submissions SET status='review',suggestions=?,reason=? WHERE id=? AND status='queued' AND started_at<>0`, string(data), reason, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit()
	}
	for admin, enabled := range p.admins {
		if enabled {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO tray_notifications(tray_id,admin_id,due_at) VALUES(?,?,?)`, id, admin, p.workerNow().Unix()); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (p *trayPhotos) processTrayNotifications(ctx context.Context) error {
	if _, err := p.store.db.ExecContext(ctx, `DELETE FROM tray_notifications WHERE NOT EXISTS (SELECT 1 FROM tray_submissions s WHERE s.id=tray_notifications.tray_id AND s.status='review')`); err != nil {
		return err
	}
	if p.notify == nil {
		return nil
	}
	rows, err := p.store.db.QueryContext(ctx, `SELECT n.tray_id,n.admin_id,n.attempts FROM tray_notifications n JOIN tray_submissions s ON s.id=n.tray_id WHERE s.status='review' AND n.due_at<=? ORDER BY n.due_at LIMIT 50`, p.workerNow().Unix())
	if err != nil {
		return err
	}
	type notification struct {
		tray, admin int64
		attempts    int
	}
	var pending []notification
	for rows.Next() {
		var n notification
		if err = rows.Scan(&n.tray, &n.admin, &n.attempts); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, n := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var status string
		err = p.store.db.QueryRowContext(ctx, `SELECT status FROM tray_submissions WHERE id=?`, n.tray).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if status != "review" {
			continue
		}
		if err = p.notify(n.admin, n.tray); err == nil {
			_, err = p.store.db.ExecContext(ctx, `DELETE FROM tray_notifications WHERE tray_id=? AND admin_id=?`, n.tray, n.admin)
		} else {
			exponent := n.attempts
			if exponent > 8 {
				exponent = 8
			}
			if exponent < 0 {
				exponent = 0
			}
			delay := time.Minute * time.Duration(1<<exponent)
			_, err = p.store.db.ExecContext(ctx, `UPDATE tray_notifications SET attempts=attempts+1,due_at=? WHERE tray_id=? AND admin_id=?`, p.workerNow().Add(delay).Unix(), n.tray, n.admin)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *trayPhotos) purgeTrayFrames(ctx context.Context) error {
	rows, err := p.store.db.QueryContext(ctx, `SELECT id,frame FROM tray_submissions WHERE status='closed' AND closed_at>0 AND closed_at<=? AND frame<>''`, p.workerNow().Add(-trayRetention).Unix())
	if err != nil {
		return err
	}
	type frame struct {
		id   int64
		name string
	}
	var frames []frame
	for rows.Next() {
		var f frame
		if err = rows.Scan(&f.id, &f.name); err != nil {
			rows.Close()
			return err
		}
		frames = append(frames, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, f := range frames {
		path, e := trayFramePath(p.frames, f.name)
		if e != nil {
			log.Printf("tray %d: %v", f.id, e)
			continue
		}
		if e = os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
			log.Printf("tray %d frame removal: %v", f.id, e)
			continue
		}
		if _, err = p.store.db.ExecContext(ctx, `UPDATE tray_submissions SET frame='' WHERE id=? AND status='closed' AND frame=?`, f.id, f.name); err != nil {
			return fmt.Errorf("clear tray frame: %w", err)
		}
	}
	return nil
}
