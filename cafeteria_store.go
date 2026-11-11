package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

func normalizedDishName(name string) string {
	return strings.Join(strings.Fields(norm.NFC.String(name)), " ")
}

func (s *SQLiteStore) initCafeteria() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS cafeteria_dishes (
			id INTEGER PRIMARY KEY, provider TEXT NOT NULL, external_id INTEGER NOT NULL,
			name TEXT NOT NULL, normalized_name TEXT NOT NULL, kind TEXT NOT NULL,
			first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL,
			UNIQUE(provider, external_id, normalized_name))`,
		`CREATE TABLE IF NOT EXISTS cafeteria_schedule (
			service_date TEXT NOT NULL, meal_type TEXT NOT NULL, position INTEGER NOT NULL,
			course TEXT NOT NULL, dish_id INTEGER NOT NULL REFERENCES cafeteria_dishes(id),
			average_score REAL NOT NULL, review_count INTEGER NOT NULL, source_url TEXT NOT NULL,
			fetched_at INTEGER NOT NULL, PRIMARY KEY(service_date, meal_type, position))`,
		`CREATE TABLE IF NOT EXISTS cafeteria_cards (
			dish_id INTEGER PRIMARY KEY REFERENCES cafeteria_dishes(id), language TEXT NOT NULL DEFAULT 'ru',
			translated_name TEXT NOT NULL DEFAULT '', short_description TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '', typical_ingredients TEXT NOT NULL DEFAULT '[]',
			sources TEXT NOT NULL DEFAULT '[]', needs_review INTEGER NOT NULL DEFAULT 1,
			review_reason TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			prompt_version TEXT NOT NULL DEFAULT '', prepared_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS cafeteria_images (
			dish_id INTEGER PRIMARY KEY REFERENCES cafeteria_dishes(id), image_url TEXT NOT NULL DEFAULT '',
			source_url TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '', license TEXT NOT NULL DEFAULT '',
			license_url TEXT NOT NULL DEFAULT '', caption TEXT NOT NULL DEFAULT '', image_kind TEXT NOT NULL DEFAULT 'example',
			status TEXT NOT NULL, checked_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS cafeteria_jobs (
			id INTEGER PRIMARY KEY, dish_id INTEGER NOT NULL REFERENCES cafeteria_dishes(id), kind TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0,
			available_at INTEGER NOT NULL, lease_until INTEGER NOT NULL DEFAULT 0, lease_token TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, UNIQUE(dish_id,kind))`,
		`CREATE INDEX IF NOT EXISTS cafeteria_jobs_due ON cafeteria_jobs(status,available_at,lease_until)`,
		`CREATE TABLE IF NOT EXISTS cafeteria_source_cooldowns (host TEXT PRIMARY KEY, until INTEGER NOT NULL)`,
	}
	for _, q := range statements {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func dishKind(name string) string {
	if strings.HasPrefix(name, "[") || strings.Contains(name, "♥") {
		return "notice"
	}
	return "dish"
}

// saveCafeteriaWeek atomically replaces all returned days, preserving each occurrence.
func (s *SQLiteStore) saveCafeteriaWeek(ctx context.Context, plans []CafeteriaWeeklyPlan, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.Unix()
	for _, plan := range plans {
		if _, err := time.Parse("2006-01-02", plan.Date); err != nil {
			return fmt.Errorf("invalid menu date %q: %w", plan.Date, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cafeteria_schedule WHERE service_date=?`, plan.Date); err != nil {
			return err
		}
		meals := make([]string, 0, len(plan.MenusByMealType))
		for meal := range plan.MenusByMealType {
			meals = append(meals, meal)
		}
		sort.Strings(meals)
		for _, meal := range meals {
			for pos, item := range plan.MenusByMealType[meal] {
				name := strings.TrimSpace(item.MenuName)
				normalized := normalizedDishName(name)
				if normalized == "" || normalized == "*" {
					continue
				}
				kind := dishKind(normalized)
				var id int64
				err := tx.QueryRowContext(ctx, `INSERT INTO cafeteria_dishes(provider,external_id,name,normalized_name,kind,first_seen,last_seen)
					VALUES('arambyeol',?,?,?,?,?,?) ON CONFLICT(provider,external_id,normalized_name)
					DO UPDATE SET name=excluded.name,last_seen=excluded.last_seen RETURNING id`, item.MenuID, name, normalized, kind, stamp, stamp).Scan(&id)
				if err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO cafeteria_schedule(service_date,meal_type,position,course,dish_id,average_score,review_count,source_url,fetched_at) VALUES(?,?,?,?,?,?,?,?,?)`,
					plan.Date, meal, pos, item.Course, id, item.AverageScore, item.ReviewCount, "https://api.arambyeol.com/plans/weekly/"+plan.Date, stamp); err != nil {
					return err
				}
				for _, job := range []string{"card", "image"} {
					if kind == "notice" && job == "image" {
						continue
					}
					if _, err = tx.ExecContext(ctx, `INSERT INTO cafeteria_jobs(dish_id,kind,available_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(dish_id,kind) DO NOTHING`, id, job, stamp, stamp); err != nil {
						return err
					}
				}
			}
		}
	}
	return tx.Commit()
}

type cafeteriaJob struct {
	ID, DishID                  int64
	Kind, Name, DishKind, Token string
	Attempts                    int
}

func (s *SQLiteStore) claimCafeteriaJob(ctx context.Context, now time.Time, token string) (*cafeteriaJob, error) {
	var j cafeteriaJob
	err := s.db.QueryRowContext(ctx, `UPDATE cafeteria_jobs SET status='running',lease_until=?,lease_token=?,attempts=attempts+1,updated_at=?
		WHERE id=(SELECT id FROM cafeteria_jobs WHERE available_at<=? AND (status='pending' OR (status='running' AND lease_until<=?))
		AND NOT EXISTS (SELECT 1 FROM cafeteria_source_cooldowns c WHERE c.until>? AND ((kind='image' AND c.host='commons.wikimedia.org') OR (kind='reference' AND c.host='ko.wikipedia.org')))
		ORDER BY available_at,id LIMIT 1)
		RETURNING id,dish_id,kind,attempts`, now.Add(5*time.Minute).Unix(), token, now.Unix(), now.Unix(), now.Unix(), now.Unix()).Scan(&j.ID, &j.DishID, &j.Kind, &j.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Token = token
	err = s.db.QueryRowContext(ctx, `SELECT name,kind FROM cafeteria_dishes WHERE id=?`, j.DishID).Scan(&j.Name, &j.DishKind)
	return &j, err
}

func (s *SQLiteStore) finishCafeteriaJob(ctx context.Context, j *cafeteriaJob, retry time.Duration, jobErr error) error {
	status, message := "done", ""
	if retry > 0 {
		status = "pending"
	}
	if jobErr != nil {
		message = jobErr.Error()
		if len(message) > 1000 {
			message = message[:1000]
		}
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `UPDATE cafeteria_jobs SET status=?,available_at=?,lease_until=0,lease_token='',last_error=?,updated_at=? WHERE id=? AND status='running' AND lease_token=?`, status, now.Add(retry).Unix(), message, now.Unix(), j.ID, j.Token)
	return err
}

func (s *SQLiteStore) enqueueReferenceRetry(ctx context.Context, dishID int64) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO cafeteria_jobs(dish_id,kind,available_at,updated_at) VALUES(?,'reference',?,?) ON CONFLICT(dish_id,kind) DO UPDATE SET status='pending',available_at=excluded.available_at,updated_at=excluded.updated_at WHERE cafeteria_jobs.status='done'`, dishID, now.Add(7*24*time.Hour).Unix(), now.Unix())
	return err
}

func (s *SQLiteStore) requeueCard(ctx context.Context, job *cafeteriaJob) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cafeteria_jobs SET status='pending',available_at=?,updated_at=? WHERE dish_id=? AND kind='card' AND status='done' AND EXISTS(SELECT 1 FROM cafeteria_jobs WHERE id=? AND status='running' AND lease_token=?)`, time.Now().Unix(), time.Now().Unix(), job.DishID, job.ID, job.Token)
	return err
}

type cafeteriaCard struct {
	TranslatedName     string   `json:"translated_name"`
	ShortDescription   string   `json:"short_description"`
	Description        string   `json:"description"`
	TypicalIngredients []string `json:"typical_ingredients"`
	NeedsReview        bool     `json:"needs_review"`
	ReviewReason       string   `json:"review_reason"`
}

func (s *SQLiteStore) saveCafeteriaCard(ctx context.Context, j *cafeteriaJob, c cafeteriaCard, sources []string, model string) error {
	ingredients, _ := json.Marshal(c.TypicalIngredients)
	references, _ := json.Marshal(sources)
	res, err := s.db.ExecContext(ctx, `INSERT INTO cafeteria_cards(dish_id,translated_name,short_description,description,typical_ingredients,sources,needs_review,review_reason,model,prompt_version,prepared_at)
		SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM cafeteria_jobs WHERE id=? AND status='running' AND lease_token=?)
		ON CONFLICT(dish_id) DO UPDATE SET translated_name=excluded.translated_name,short_description=excluded.short_description,description=excluded.description,typical_ingredients=excluded.typical_ingredients,sources=excluded.sources,needs_review=excluded.needs_review,review_reason=excluded.review_reason,model=excluded.model,prompt_version=excluded.prompt_version,prepared_at=excluded.prepared_at`,
		j.DishID, c.TranslatedName, c.ShortDescription, c.Description, string(ingredients), string(references), c.NeedsReview, c.ReviewReason, model, "v1", time.Now().Unix(), j.ID, j.Token)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("job lease expired")
	}
	return err
}

type cafeteriaImage struct{ URL, SourceURL, Author, License, LicenseURL, Caption, Status string }

func (s *SQLiteStore) saveCafeteriaImage(ctx context.Context, j *cafeteriaJob, image cafeteriaImage) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO cafeteria_images(dish_id,image_url,source_url,author,license,license_url,caption,status,checked_at)
		SELECT ?,?,?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM cafeteria_jobs WHERE id=? AND status='running' AND lease_token=?)
		ON CONFLICT(dish_id) DO UPDATE SET image_url=excluded.image_url,source_url=excluded.source_url,author=excluded.author,license=excluded.license,license_url=excluded.license_url,caption=excluded.caption,status=excluded.status,checked_at=excluded.checked_at`,
		j.DishID, image.URL, image.SourceURL, image.Author, image.License, image.LicenseURL, image.Caption, image.Status, time.Now().Unix(), j.ID, j.Token)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("job lease expired")
	}
	return err
}
