package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"time"

	"telegram-gemini-bot/internal/llm"
)

const (
	trayRetention = 30 * 24 * time.Hour
	trayMealLimit = 3
)

type trayPhotos struct {
	uploadSlots chan struct{}
	store       *SQLiteStore
	frames      string
	admins      map[int64]bool
	now         func() time.Time
	ai          interface {
		Generate(context.Context, llm.Request) (llm.Response, error)
	}
	model      string
	dailyCalls int
	notify     func(int64, int64) error
}

type trayDish struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	PhotoID string `json:"photo_id"`
}

type trayCrop struct {
	DishID   int64  `json:"dish_id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
	Selected bool   `json:"selected"`
	Reason   string `json:"reason"`
	Replace  string `json:"replace"`
}

type traySummary struct {
	ID          int64      `json:"id"`
	Date        string     `json:"date"`
	Meal        string     `json:"meal"`
	Status      string     `json:"status"`
	ScanRunning bool       `json:"scan_running,omitempty"`
	CanRescan   bool       `json:"can_rescan,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Result      []string   `json:"result"`
	Dishes      []trayDish `json:"dishes,omitempty"`
	Suggestions []trayCrop `json:"suggestions,omitempty"`
}

const trayColumns = "id,service_date,meal,status,reason,result,status='queued' AND started_at<>0"

var (
	errTrayDuplicate     = errors.New("this file was already submitted for this meal")
	errTrayQuota         = errors.New("daily frame limit reached")
	errTrayMealLimit     = errors.New("shared meal frame limit reached")
	errTrayMenu          = errors.New("meal does not have dishes")
	errTrayConflict      = errors.New("menu, photo or submission changed; reload and review")
	errTrayCrop          = errors.New("invalid or duplicate crop")
	errTrayAIUnavailable = errors.New("ИИ-скан сейчас недоступен.")
	errTrayAIBudget      = errors.New("Дневной лимит ИИ-сканов исчерпан. Попробуйте завтра или проверьте фото вручную.")
	errTrayFrameMissing  = errors.New("Исходное фото недоступно для повторного скана.")
)

func initTrayPhotos(store *SQLiteStore) error {
	_, err := store.db.Exec(`
	CREATE TABLE IF NOT EXISTS tray_submissions (
		id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL,
		service_date TEXT NOT NULL, meal TEXT NOT NULL, upload_day TEXT NOT NULL,
		digest TEXT NOT NULL, frame TEXT NOT NULL, dishes TEXT NOT NULL,
		suggestions TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '[]',
		created_at INTEGER NOT NULL, closed_at INTEGER NOT NULL DEFAULT 0,
		started_at INTEGER NOT NULL DEFAULT 0, UNIQUE(service_date,meal,digest));
	CREATE INDEX IF NOT EXISTS tray_quota ON tray_submissions(user_id,upload_day);
	CREATE INDEX IF NOT EXISTS tray_status ON tray_submissions(status,id);
	CREATE TABLE IF NOT EXISTS tray_notifications (
		tray_id INTEGER NOT NULL, admin_id INTEGER NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0, due_at INTEGER NOT NULL,
		PRIMARY KEY(tray_id,admin_id));
	CREATE TABLE IF NOT EXISTS tray_ai_calls(day TEXT PRIMARY KEY,calls INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS tray_ai_history (
		id INTEGER PRIMARY KEY, tray_id INTEGER NOT NULL REFERENCES tray_submissions(id),
		started_at INTEGER NOT NULL, attempt INTEGER NOT NULL, model TEXT NOT NULL,
		finish_reason TEXT NOT NULL, reason TEXT NOT NULL, response TEXT NOT NULL,
		response_truncated INTEGER NOT NULL);
	CREATE INDEX IF NOT EXISTS tray_ai_history_recent ON tray_ai_history(tray_id,started_at DESC,id DESC);
	CREATE INDEX IF NOT EXISTS tray_ai_history_expiry ON tray_ai_history(started_at);
	CREATE TABLE IF NOT EXISTS tray_published (
		dish_id INTEGER PRIMARY KEY REFERENCES cafeteria_dishes(id), photo_id TEXT NOT NULL UNIQUE,
		jpeg BLOB NOT NULL);
	`)
	return err
}

type trayQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func trayMenuDishes(ctx context.Context, q trayQueryer, date, meal string) ([]trayDish, error) {
	rows, err := q.QueryContext(ctx, `SELECT d.id,COALESCE(NULLIF(c.translated_name,''),d.name),COALESCE(p.photo_id,'')
		FROM cafeteria_schedule s JOIN cafeteria_dishes d ON s.dish_id=d.id
		LEFT JOIN cafeteria_cards c ON c.dish_id=d.id LEFT JOIN tray_published p ON p.dish_id=d.id
		WHERE s.service_date=? AND s.meal_type=? AND d.kind='dish' GROUP BY d.id ORDER BY MIN(s.position)`, date, meal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dishes []trayDish
	for rows.Next() {
		var d trayDish
		if err := rows.Scan(&d.ID, &d.Name, &d.PhotoID); err != nil {
			return nil, err
		}
		dishes = append(dishes, d)
	}
	return dishes, rows.Err()
}

func trayPhotoID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (p *trayPhotos) upload(ctx context.Context, user int64, date, meal, digest string, img image.Image) (traySummary, error) {
	// Encode before acquiring the SQLite connection; the file goes live only
	// together with its submission. The worker can reap unreferenced files.
	frame := trayPhotoID() + ".jpg"
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, img, &jpeg.Options{Quality: 90}); err != nil {
		return traySummary{}, err
	}
	now := p.now()
	day := now.In(seoulLocation).Format("2006-01-02")
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return traySummary{}, err
	}
	defer tx.Rollback()
	// Check both quotas on the same SQLite connection and transaction as the insert.
	var exists, userCount, mealCount int
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tray_submissions WHERE service_date=? AND meal=? AND digest=?),
		(SELECT COUNT(*) FROM tray_submissions WHERE user_id=? AND upload_day=?),
		(SELECT COUNT(*) FROM tray_submissions WHERE service_date=? AND meal=?)`,
		date, meal, digest, user, day, date, meal).Scan(&exists, &userCount, &mealCount)
	switch {
	case err != nil:
		return traySummary{}, err
	case exists != 0:
		return traySummary{}, errTrayDuplicate
	case mealCount >= trayMealLimit:
		return traySummary{}, errTrayMealLimit
	case userCount >= 3:
		return traySummary{}, errTrayQuota
	}
	dishes, err := trayMenuDishes(ctx, tx, date, meal)
	if err != nil {
		return traySummary{}, err
	}
	if len(dishes) == 0 {
		return traySummary{}, errTrayMenu
	}
	for i := range dishes {
		dishes[i].PhotoID = ""
	}
	snapshot, _ := json.Marshal(dishes)
	sub := traySummary{Date: date, Meal: meal, Status: "queued", Result: []string{}}
	if !p.trayAIEnabled() {
		sub.Status = "review"
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO tray_submissions(user_id,service_date,meal,upload_day,digest,frame,dishes,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, user, date, meal, day, digest, frame, string(snapshot), sub.Status, now.Unix()).Scan(&sub.ID)
	if err != nil {
		return traySummary{}, err
	}
	if sub.Status == "review" {
		for admin := range p.admins {
			if _, err := tx.ExecContext(ctx, `INSERT INTO tray_notifications(tray_id,admin_id,due_at) VALUES(?,?,?)`, sub.ID, admin, now.Unix()); err != nil {
				return traySummary{}, err
			}
		}
	}
	path := filepath.Join(p.frames, frame)
	if err := os.WriteFile(path, jpegBytes.Bytes(), 0600); err != nil {
		_ = os.Remove(path)
		return traySummary{}, err
	}
	// On an ambiguous commit error leave the frame for the orphan sweep.
	return sub, tx.Commit()
}

func validTrayCrop(c trayCrop) bool {
	return trayCropIssue(c) == ""
}

func trayCropIssue(c trayCrop) string {
	switch {
	case c.X < 0 || c.X >= 1000:
		return "координата слева (x) должна быть от 0 до 999"
	case c.Y < 0 || c.Y >= 1000:
		return "координата сверху (y) должна быть от 0 до 999"
	case c.W <= 0:
		return "ширина (w) должна быть больше нуля"
	case c.H <= 0:
		return "высота (h) должна быть больше нуля"
	case c.W > 1000-c.X:
		return "прямоугольник выходит за правый край фото (x + w > 1000)"
	case c.H > 1000-c.Y:
		return "прямоугольник выходит за нижний край фото (y + h > 1000)"
	default:
		return ""
	}
}

func (p *trayPhotos) requeueTray(ctx context.Context, id int64) error {
	if !p.trayAIEnabled() {
		return errTrayAIUnavailable
	}
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, frame string
	if err := tx.QueryRowContext(ctx, `SELECT status,frame FROM tray_submissions WHERE id=?`, id).Scan(&status, &frame); err != nil {
		return err
	}
	if status != "review" {
		return errTrayConflict
	}
	path, err := trayFramePath(p.frames, frame)
	if err != nil {
		return errTrayFrameMissing
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return errTrayFrameMissing
	}
	var calls int
	day, limit := p.trayCallQuota()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT calls FROM tray_ai_calls WHERE day=?),0)`, day).Scan(&calls); err != nil {
		return err
	}
	if calls >= limit {
		return errTrayAIBudget
	}
	// Reset the claim only for a new, explicit scan. The worker still reserves
	// the call against the daily limit and never repeats an interrupted claim.
	if _, err := tx.ExecContext(ctx, `UPDATE tray_submissions SET status='queued',started_at=0,suggestions='[]',reason='' WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tray_notifications WHERE tray_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *trayPhotos) decide(ctx context.Context, id int64, crops []trayCrop, rotation int) error {
	if rotation < 0 || rotation > 3 {
		return errTrayCrop
	}
	seen := map[int64]bool{}
	for _, c := range crops {
		if !validTrayCrop(c) || c.DishID <= 0 || seen[c.DishID] {
			return errTrayCrop
		}
		seen[c.DishID] = true
	}
	var frame, status string
	if err := p.store.db.QueryRowContext(ctx, `SELECT frame,status FROM tray_submissions WHERE id=?`, id).Scan(&frame, &status); err != nil {
		return err
	}
	if status != "review" {
		return errTrayConflict
	}
	var encoded [][]byte
	if len(crops) > 0 {
		f, err := os.Open(filepath.Join(p.frames, frame))
		if err != nil {
			return err
		}
		img, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			return err
		}
		if rotation != 0 {
			w, h := img.Bounds().Dx(), img.Bounds().Dy()
			if rotation%2 == 1 {
				w, h = h, w
			}
			// EXIF orientations 6, 3 and 8 are clockwise quarter turns.
			img = sampleTrayImage(img, w, h, [...]int{1, 6, 3, 8}[rotation])
		}
		b := img.Bounds()
		for _, c := range crops {
			r := image.Rect(b.Min.X+b.Dx()*c.X/1000, b.Min.Y+b.Dy()*c.Y/1000,
				b.Min.X+(b.Dx()*(c.X+c.W)+999)/1000, b.Min.Y+(b.Dy()*(c.Y+c.H)+999)/1000)
			part := img.(interface {
				SubImage(image.Rectangle) image.Image
			}).SubImage(r)
			var out bytes.Buffer
			if err := jpeg.Encode(&out, fitTrayImage(part, 768), &jpeg.Options{Quality: 88}); err != nil {
				return err
			}
			encoded = append(encoded, out.Bytes())
		}
	}
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var date, meal, snapshot string
	err = tx.QueryRowContext(ctx, `SELECT service_date,meal,dishes FROM tray_submissions WHERE id=? AND status='review'`, id).Scan(&date, &meal, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return errTrayConflict
	}
	if err != nil {
		return err
	}
	current, err := trayMenuDishes(ctx, tx, date, meal)
	if err != nil {
		return err
	}
	var original []trayDish
	if err := json.Unmarshal([]byte(snapshot), &original); err != nil {
		return err
	}
	allowed := map[int64]bool{}
	for _, d := range original {
		allowed[d.ID] = true
	}
	dishes := map[int64]trayDish{}
	for _, d := range current {
		if allowed[d.ID] {
			dishes[d.ID] = d
		}
	}
	names := []string{}
	for i, c := range crops {
		d, ok := dishes[c.DishID]
		if !ok || d.PhotoID != c.Replace {
			return errTrayConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tray_published(dish_id,photo_id,jpeg) VALUES(?,?,?)
			ON CONFLICT(dish_id) DO UPDATE SET photo_id=excluded.photo_id,jpeg=excluded.jpeg`, c.DishID, trayPhotoID(), encoded[i]); err != nil {
			return err
		}
		names = append(names, d.Name)
	}
	result, _ := json.Marshal(names)
	if _, err := tx.ExecContext(ctx, `UPDATE tray_submissions SET status='closed',closed_at=?,result=? WHERE id=?`, p.now().Unix(), string(result), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tray_notifications WHERE tray_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
