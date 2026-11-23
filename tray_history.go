package main

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	trayHistoryRetention = 30 * 24 * time.Hour
	trayHistoryLimit     = 20
	trayHistoryTextLimit = 16 << 10
	trayHistoryDBTimeout = time.Second
)

type trayHistoryEntry struct {
	StartedAt         time.Time `json:"started_at"`
	Attempt           int       `json:"attempt"`
	Model             string    `json:"model"`
	FinishReason      string    `json:"finish_reason"`
	Reason            string    `json:"reason"`
	Response          string    `json:"response"`
	ResponseTruncated bool      `json:"response_truncated"`
}

func (p *trayPhotos) recordTrayHistory(ctx context.Context, trayID int64, entry trayHistoryEntry) {
	text := strings.ToValidUTF8(entry.Response, "\uFFFD")
	truncated := len(text) > trayHistoryTextLimit
	if truncated {
		n := trayHistoryTextLimit
		for !utf8.RuneStart(text[n]) {
			n--
		}
		text = text[:n]
	}

	// A cancelled Generate is still an attempt. Bound the best-effort write
	// independently of the scan context and do not change the scan outcome.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trayHistoryDBTimeout)
	defer cancel()
	_, err := p.store.db.ExecContext(writeCtx, `INSERT INTO tray_ai_history
		(tray_id,started_at,attempt,model,finish_reason,reason,response,response_truncated)
		VALUES(?,?,?,?,?,?,?,?)`, trayID, entry.StartedAt.UnixNano(), entry.Attempt,
		entry.Model, entry.FinishReason, entry.Reason, text, truncated)
	if err != nil {
		log.Printf("tray %d history write: %v", trayID, err)
	}
}

func (p *trayPhotos) trayHistoryCutoff() int64 {
	return p.workerNow().Add(-trayHistoryRetention).UnixNano()
}

func (p *trayPhotos) purgeTrayHistory(ctx context.Context) {
	purgeCtx, cancel := context.WithTimeout(ctx, trayHistoryDBTimeout)
	defer cancel()
	if _, err := p.store.db.ExecContext(purgeCtx, `DELETE FROM tray_ai_history WHERE started_at<=?`, p.trayHistoryCutoff()); err != nil && ctx.Err() == nil {
		log.Printf("tray history cleanup: %v", err)
	}
}

func (p *trayPhotos) listTrayHistory(ctx context.Context, trayID int64) ([]trayHistoryEntry, error) {
	var exists bool
	if err := p.store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tray_submissions WHERE id=?)`, trayID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}
	rows, err := p.store.db.QueryContext(ctx, `SELECT started_at,attempt,model,finish_reason,reason,response,response_truncated
		FROM tray_ai_history WHERE tray_id=? AND started_at>?
		ORDER BY started_at DESC,id DESC LIMIT ?`, trayID, p.trayHistoryCutoff(), trayHistoryLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []trayHistoryEntry{}
	for rows.Next() {
		var entry trayHistoryEntry
		var stamp int64
		if err := rows.Scan(&stamp, &entry.Attempt, &entry.Model, &entry.FinishReason,
			&entry.Reason, &entry.Response, &entry.ResponseTruncated); err != nil {
			return nil, err
		}
		entry.StartedAt = time.Unix(0, stamp).UTC()
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
