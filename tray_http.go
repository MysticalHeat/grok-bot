package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (p *trayPhotos) register(mux *http.ServeMux, token string) {
	admin := func(r *http.Request) bool {
		return p.admins[telegramUserID(r.Header.Get("X-Telegram-Init-Data"), token, p.now())]
	}
	mux.HandleFunc("GET /api/tray/published", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !admin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		p.listPublished(w, r)
	})
	mux.HandleFunc("GET /api/tray/publications", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !admin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		p.listPublicationHistory(w, r)
	})
	mux.HandleFunc("GET /api/tray-photo/{photo}", func(w http.ResponseWriter, r *http.Request) {
		var jpeg []byte
		err := p.store.db.QueryRowContext(r.Context(), `SELECT jpeg FROM tray_published WHERE photo_id=?`, r.PathValue("photo")).Scan(&jpeg)
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			trayHTTPError(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(jpeg)
	})
	mux.HandleFunc("/api/tray", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		user := telegramUserID(r.Header.Get("X-Telegram-Init-Data"), token, p.now())
		if user == 0 {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.Method {
		case http.MethodGet:
			p.list(w, r, user)
		case http.MethodPost:
			p.receive(w, r, user)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", 405)
		}
	})
	mux.HandleFunc("GET /api/tray/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		user := telegramUserID(r.Header.Get("X-Telegram-Init-Data"), token, p.now())
		if user == 0 {
			http.Error(w, "unauthorized", 401)
			return
		}
		p.detail(w, r, user)
	})
	protected := func(pattern string, fn func(http.ResponseWriter, *http.Request, int64)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if !admin(r) {
				http.Error(w, "forbidden", 403)
				return
			}
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id <= 0 {
				http.NotFound(w, r)
				return
			}
			fn(w, r, id)
		})
	}
	protected("GET /api/tray/{id}/frame", p.serveFrame)
	protected("GET /api/tray/{id}/history", func(w http.ResponseWriter, r *http.Request, id int64) {
		entries, err := p.listTrayHistory(r.Context(), id)
		if err != nil {
			trayHTTPError(w, err)
			return
		}
		trayJSON(w, http.StatusOK, entries)
	})
	protected("POST /api/tray/{id}/decision", p.decideHTTP)
	protected("POST /api/tray/{id}/republish", func(w http.ResponseWriter, r *http.Request, id int64) {
		p.publishTrayHTTP(w, r, id, true)
	})
	protected("POST /api/tray/{id}/rescan", func(w http.ResponseWriter, r *http.Request, id int64) {
		if err := p.requeueTray(r.Context(), id); err != nil {
			trayHTTPError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	protected("DELETE /api/menu/dish/{id}/image", func(w http.ResponseWriter, r *http.Request, id int64) {
		query := r.URL.Query()
		photoID, imageURL := query.Get("photo_id"), query.Get("image_url")
		if (photoID == "") == (imageURL == "") {
			http.Error(w, "provide exactly one image identifier", http.StatusBadRequest)
			return
		}
		if err := p.store.removeMenuDishImage(r.Context(), id, photoID, imageURL, p.now()); err != nil {
			trayHTTPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	protected("DELETE /api/tray/dish/{id}", func(w http.ResponseWriter, r *http.Request, id int64) {
		res, err := p.store.db.ExecContext(r.Context(), `DELETE FROM tray_published WHERE dish_id=? AND photo_id=?`, id, r.URL.Query().Get("photo_id"))
		if err != nil {
			trayHTTPError(w, err)
			return
		}
		n, err := res.RowsAffected()
		if err != nil {
			trayHTTPError(w, err)
			return
		}
		if n == 0 {
			trayHTTPError(w, errTrayConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (p *trayPhotos) listPublicationHistory(w http.ResponseWriter, r *http.Request) {
	before := int64(math.MaxInt64)
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		var err error
		before, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || before <= 0 {
			http.Error(w, "invalid publication cursor", http.StatusBadRequest)
			return
		}
	}
	rows, err := p.store.db.QueryContext(r.Context(), `SELECT id,service_date,meal,status,result,frame,closed_at
		FROM tray_submissions WHERE status='closed' AND result<>'[]' AND id<? ORDER BY id DESC LIMIT 50`, before)
	if err != nil {
		trayHTTPError(w, err)
		return
	}
	defer rows.Close()
	items := []traySummary{}
	for rows.Next() {
		var s traySummary
		var result, frame string
		var closedAt int64
		if err := rows.Scan(&s.ID, &s.Date, &s.Meal, &s.Status, &result, &frame, &closedAt); err != nil {
			trayHTTPError(w, err)
			return
		}
		if err := json.Unmarshal([]byte(result), &s.Result); err != nil {
			trayHTTPError(w, err)
			return
		}
		s.FrameAvailable = p.publicationFrameAvailable(frame, closedAt)
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		trayHTTPError(w, err)
		return
	}
	trayJSON(w, http.StatusOK, items)
}

func (p *trayPhotos) listPublished(w http.ResponseWriter, r *http.Request) {
	rows, err := p.store.db.QueryContext(r.Context(), `SELECT d.id, COALESCE(NULLIF(c.translated_name,''),d.name), p.photo_id
		FROM tray_published p JOIN cafeteria_dishes d ON d.id=p.dish_id
		LEFT JOIN cafeteria_cards c ON c.dish_id=d.id ORDER BY 2,d.id`)
	if err != nil {
		trayHTTPError(w, err)
		return
	}
	defer rows.Close()
	items := []trayDish{}
	for rows.Next() {
		var d trayDish
		if err := rows.Scan(&d.ID, &d.Name, &d.PhotoID); err != nil {
			trayHTTPError(w, err)
			return
		}
		items = append(items, d)
	}
	if err := rows.Err(); err != nil {
		trayHTTPError(w, err)
		return
	}
	trayJSON(w, http.StatusOK, items)
}

func trayJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func trayHTTPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "not found", 404)
	case errors.Is(err, errTrayMealLimit):
		trayJSON(w, http.StatusTooManyRequests, map[string]string{"code": "meal_photo_limit"})
	case errors.Is(err, errTrayAIBudget):
		trayJSON(w, http.StatusTooManyRequests, map[string]string{"code": "tray_ai_budget", "message": errTrayAIBudget.Error()})
	case errors.Is(err, errTrayAIUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, errTrayFrameMissing):
		http.Error(w, err.Error(), http.StatusGone)
	case errors.Is(err, errTrayConflict), errors.Is(err, errTrayDuplicate):
		http.Error(w, err.Error(), 409)
	case errors.Is(err, errTrayMenu), errors.Is(err, errTrayCrop):
		http.Error(w, err.Error(), 400)
	default:
		log.Printf("tray photos: %v", err)
		http.Error(w, "internal error", 500)
	}
}

func (p *trayPhotos) receive(w http.ResponseWriter, r *http.Request, user int64) {
	date, meal := r.URL.Query().Get("date"), r.URL.Query().Get("meal")
	if _, err := time.Parse(time.DateOnly, date); err != nil || len(meal) == 0 || len(meal) > 64 {
		http.Error(w, "invalid date or meal", 400)
		return
	}
	if p.uploadSlots != nil {
		select {
		case p.uploadSlots <- struct{}{}:
			defer func() { <-p.uploadSlots }()
		default:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "upload service busy", http.StatusServiceUnavailable)
			return
		}
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, trayUploadBytes))
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		http.Error(w, "frame too large", 413)
		return
	}
	if err != nil {
		http.Error(w, "could not read frame", 400)
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		http.Error(w, "expected a JPEG or PNG", 415)
		return
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 10000 || cfg.Height > 10000 || int64(cfg.Width)*int64(cfg.Height) > 25_000_000 {
		http.Error(w, "frame dimensions too large", 413)
		return
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		http.Error(w, "invalid frame", 415)
		return
	}
	sum := sha256.Sum256(data)
	sub, err := p.upload(r.Context(), user, date, meal, hex.EncodeToString(sum[:]), normalizeTrayImage(img, trayOrientation(data, format)))
	if err != nil {
		trayHTTPError(w, err)
		return
	}
	trayJSON(w, 201, sub)
}

func (p *trayPhotos) list(w http.ResponseWriter, r *http.Request, user int64) {
	query := `SELECT ` + trayColumns + ` FROM tray_submissions WHERE user_id=? ORDER BY id DESC LIMIT 100`
	args := []any{user}
	moderating := r.URL.Query().Get("moderation") == "1"
	if moderating {
		if !p.admins[user] {
			http.Error(w, "forbidden", 403)
			return
		}
		query = `SELECT ` + trayColumns + ` FROM tray_submissions WHERE status IN ('review','queued') ORDER BY status='review' DESC,id LIMIT 100`
		args = nil
	}
	rows, err := p.store.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		trayHTTPError(w, err)
		return
	}
	defer rows.Close()
	items := []traySummary{}
	for rows.Next() {
		var s traySummary
		var result string
		if err := rows.Scan(&s.ID, &s.Date, &s.Meal, &s.Status, &s.Reason, &result, &s.ScanRunning); err != nil {
			trayHTTPError(w, err)
			return
		}
		if !moderating {
			s.Reason = ""
		}
		if err := json.Unmarshal([]byte(result), &s.Result); err != nil {
			trayHTTPError(w, err)
			return
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		trayHTTPError(w, err)
		return
	}
	trayJSON(w, 200, items)
}

func (p *trayPhotos) detail(w http.ResponseWriter, r *http.Request, user int64) {
	var s traySummary
	var result, dishes, suggestions, frame string
	var owner, closedAt int64
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = p.store.db.QueryRowContext(r.Context(), `SELECT `+trayColumns+`,user_id,dishes,suggestions,frame,closed_at FROM tray_submissions WHERE id=?`, id).
		Scan(&s.ID, &s.Date, &s.Meal, &s.Status, &s.Reason, &result, &s.ScanRunning, &owner, &dishes, &suggestions, &frame, &closedAt)
	if err != nil {
		trayHTTPError(w, err)
		return
	}
	if owner != user && !p.admins[user] {
		http.NotFound(w, r)
		return
	}
	if err := json.Unmarshal([]byte(result), &s.Result); err != nil {
		trayHTTPError(w, err)
		return
	}
	if p.admins[user] {
		s.CanRescan = s.Status == "review" && p.trayAIEnabled()
		if s.Status == "closed" && len(s.Result) != 0 {
			s.FrameAvailable = p.publicationFrameAvailable(frame, closedAt)
			s.PublicationCrops, err = p.listPublicationCrops(r.Context(), id)
			if err != nil {
				trayHTTPError(w, err)
				return
			}
		}
		var snapshot []trayDish
		if err := json.Unmarshal([]byte(dishes), &snapshot); err != nil {
			// The worker may have transitioned to review precisely because
			// the snapshot was invalid. Keep its diagnostic accessible.
			trayJSON(w, 200, s)
			return
		}
		if err := json.Unmarshal([]byte(suggestions), &s.Suggestions); err != nil {
			trayHTTPError(w, err)
			return
		}
		current, err := trayMenuDishes(r.Context(), p.store.db, s.Date, s.Meal)
		if err != nil {
			trayHTTPError(w, err)
			return
		}
		allowed := map[int64]bool{}
		for _, d := range snapshot {
			allowed[d.ID] = true
		}
		active := map[int64]bool{}
		for _, d := range current {
			if allowed[d.ID] {
				s.Dishes = append(s.Dishes, d)
				active[d.ID] = true
			}
		}
		for i := range s.Suggestions {
			if !active[s.Suggestions[i].DishID] {
				s.Suggestions[i].DishID = 0
				s.Suggestions[i].Selected = false
			}
		}
	} else {
		s.Reason = ""
	}
	trayJSON(w, 200, s)
}

func (p *trayPhotos) serveFrame(w http.ResponseWriter, r *http.Request, id int64) {
	var frame string
	err := p.store.db.QueryRowContext(r.Context(), `SELECT frame FROM tray_submissions WHERE id=? AND frame<>'' AND
		(status<>'closed' OR closed_at>?)`, id, p.now().Add(-trayRetention).Unix()).Scan(&frame)
	if err != nil {
		trayHTTPError(w, err)
		return
	}
	file, err := os.Open(filepath.Join(p.frames, frame))
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		trayHTTPError(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, file)
}

func (p *trayPhotos) decideHTTP(w http.ResponseWriter, r *http.Request, id int64) {
	p.publishTrayHTTP(w, r, id, false)
}

func (p *trayPhotos) publishTrayHTTP(w http.ResponseWriter, r *http.Request, id int64, repeat bool) {
	var input struct {
		Crops    *[]trayCrop     `json:"crops"`
		Rotation json.RawMessage `json:"rotation"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	if err := dec.Decode(&input); err != nil || input.Crops == nil || len(*input.Crops) > 32 || dec.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid decision", 400)
		return
	}
	rotation := 0
	if input.Rotation != nil && (string(input.Rotation) == "null" || json.Unmarshal(input.Rotation, &rotation) != nil || rotation < 0 || rotation > 3) {
		http.Error(w, "invalid decision", 400)
		return
	}
	for _, c := range *input.Crops {
		if len(c.Replace) > 32 || strings.ContainsAny(c.Replace, "/\\") {
			trayHTTPError(w, errTrayCrop)
			return
		}
	}
	if err := p.publishTray(r.Context(), id, *input.Crops, rotation, repeat); err != nil {
		trayHTTPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
