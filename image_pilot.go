package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"telegram-gemini-bot/internal/llm"
)

const (
	pilotVersion     = "vision-pilot-v1"
	pilotBudget      = 5.0
	pilotReservation = 0.10
	maxPilotImages   = 10
)

type pilotCandidate struct {
	DishID                                                               int64
	Name, Translation, Caption, URL, Source, Author, License, LicenseURL string
	Thumb, State, Decision, Reason, Model                                string
	Cost                                                                 sql.NullFloat64
}

// The pilot is deliberately separate from the menu worker: model decisions
// never publish images, and the existing menu is unaffected by a pilot run.
func runImagePilotCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: image-pilot /absolute/path/to/report.html")
	}
	if err := loadEnvFile(); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	store, err := NewSQLiteStore(cfg.SQLitePath, cfg.HistoryLimit)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	model, err := llm.NewClient(ctx, llmConfig(cfg))
	if err != nil {
		return err
	}
	p := newCafeteriaPreparer(store, &http.Client{Timeout: 20 * time.Second}, cfg)
	p.model = model
	err = p.runImagePilot(ctx)
	reportErr := writeImagePilotReport(ctx, store, args[0])
	if reportErr != nil {
		return fmt.Errorf("pilot: %v; report: %w", err, reportErr)
	}
	return err
}

func (p *cafeteriaPreparer) runImagePilot(ctx context.Context) error {
	// Existing candidates let us compare model judgments against real failures
	// before spending anything on broader searches or publishing changes.
	rows, err := p.store.db.QueryContext(ctx, `SELECT DISTINCT d.id,d.name,COALESCE(c.translated_name,''),i.caption,i.image_url,i.source_url,i.author,i.license,i.license_url
		FROM cafeteria_schedule s JOIN cafeteria_dishes d ON d.id=s.dish_id
		JOIN cafeteria_images i ON i.dish_id=d.id LEFT JOIN cafeteria_cards c ON c.dish_id=d.id
		WHERE s.service_date=? AND d.kind='dish' AND i.image_url<>''
		ORDER BY CASE d.name WHEN '쌀밥' THEN 0 WHEN '우동장국' THEN 1 ELSE 2 END, d.name LIMIT ?`, time.Now().In(seoulLocation).Format("2006-01-02"), maxPilotImages)
	if err != nil {
		return err
	}
	var candidates []pilotCandidate
	for rows.Next() {
		var c pilotCandidate
		if err := rows.Scan(&c.DishID, &c.Name, &c.Translation, &c.Caption, &c.URL, &c.Source, &c.Author, &c.License, &c.LicenseURL); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(candidates) == 0 {
		return errors.New("no existing images for today's menu")
	}
	for _, candidate := range candidates {
		var exists int
		if err := p.store.db.QueryRowContext(ctx, `SELECT count(*) FROM cafeteria_image_pilot WHERE dish_id=? AND caption=? AND model=? AND prompt_version=?`, candidate.DishID, candidate.Caption, p.config.CafeteriaModel, pilotVersion).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		thumb, image, mime, err := p.pilotThumbnail(ctx, candidate.Caption)
		if err != nil {
			return fmt.Errorf("thumbnail %s: %w", candidate.Name, err)
		}
		candidate.Thumb = thumb
		id, err := reservePilotRequest(ctx, p.store, candidate, p.config.CafeteriaModel)
		if err != nil {
			return err
		}
		prompt, _ := json.Marshal(map[string]string{"original_name": candidate.Name, "translated_name": candidate.Translation, "file_title": candidate.Caption})
		schema := &llm.Schema{Type: "object", Required: []string{"decision", "reason"}, Properties: map[string]*llm.Schema{"decision": {Type: "string"}, "reason": {Type: "string"}}}
		instruction := `Assess whether this image is a suitable example of the specific cafeteria dish. Compare visible food and the provided title with the dish; reject clear contradictions, such as beans in plain rice or a dry dish for broth. Do not assume ingredients you cannot see. Return JSON: decision must be approve, reject, or review; reason must explain the decision briefly in Russian. Dish data: `
		result, err := p.model.Generate(ctx, llm.Request{Model: p.config.CafeteriaModel, MaxTokens: 512, ResponseSchema: schema, Contents: []llm.Content{{Role: "user", Parts: []llm.Part{{Text: instruction + string(prompt)}, {Data: image, MimeType: mime}}}}})
		if err != nil {
			return fmt.Errorf("model request for %s: %w; budget reservation retained pending reconciliation", candidate.Name, err)
		}
		if result.Usage == nil || result.Usage.Cost == nil || *result.Usage.Cost < 0 {
			return errors.New("OpenRouter omitted cost; pilot stopped with reservation retained")
		}
		var verdict struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		parseErr := json.Unmarshal([]byte(result.Text), &verdict)
		if parseErr != nil || (verdict.Decision != "approve" && verdict.Decision != "reject" && verdict.Decision != "review") || strings.TrimSpace(verdict.Reason) == "" {
			verdict.Decision = "review"
			verdict.Reason = "Некорректный ответ модели; требуется ручная проверка"
		}
		state := "completed"
		if *result.Usage.Cost > pilotReservation {
			state = "overrun"
		}
		_, err = p.store.db.ExecContext(ctx, `UPDATE cafeteria_image_pilot SET state=?,decision=?,reason=?,cost_usd=?,prompt_tokens=?,completion_tokens=?,updated_at=? WHERE id=? AND state='reserved'`, state, verdict.Decision, verdict.Reason, *result.Usage.Cost, result.Usage.PromptTokens, result.Usage.CompletionTokens, time.Now().Unix(), id)
		if err != nil {
			return err
		}
		if *result.Usage.Cost > pilotReservation {
			return errors.New("request exceeded reserved budget; pilot stopped")
		}
	}
	return nil
}

func reservePilotRequest(ctx context.Context, store *SQLiteStore, c pilotCandidate, model string) (int64, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var spent float64
	var uncertain, count int
	err = tx.QueryRowContext(ctx, `SELECT (SELECT COALESCE(SUM(COALESCE(cost_usd,reserved_usd)),0) FROM cafeteria_image_pilot) + (SELECT COALESCE(SUM(COALESCE(cost_usd,reserved_usd)),0) FROM cafeteria_image_spend),
		(SELECT count(*) FROM cafeteria_image_pilot WHERE state NOT IN ('completed','estimated')) + (SELECT count(*) FROM cafeteria_image_spend WHERE state NOT IN ('completed','estimated')),
		(SELECT count(*) FROM cafeteria_image_pilot WHERE prompt_version=?)`, pilotVersion).Scan(&spent, &uncertain, &count)
	if err != nil {
		return 0, err
	}
	if uncertain > 0 {
		return 0, errors.New("unreconciled pilot request; refusing further spending")
	}
	if count >= maxPilotImages || spent+pilotReservation > pilotBudget {
		return 0, errors.New("pilot request count or budget exhausted")
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO cafeteria_image_pilot(dish_id,caption,image_url,source_url,author,license,license_url,thumbnail_url,model,prompt_version,state,reserved_usd,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'reserved',?,?)`, c.DishID, c.Caption, c.URL, c.Source, c.Author, c.License, c.LicenseURL, c.Thumb, model, pilotVersion, pilotReservation, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (p *cafeteriaPreparer) pilotThumbnail(ctx context.Context, title string) (string, []byte, string, error) {
	var data wikiResponse
	params := url.Values{"action": {"query"}, "format": {"json"}, "formatversion": {"2"}, "titles": {title}, "prop": {"imageinfo"}, "iiprop": {"url|mime"}, "iiurlwidth": {"320"}}
	if err := p.getJSON(ctx, "https://commons.wikimedia.org/w/api.php", params, &data); err != nil {
		return "", nil, "", err
	}
	if data.Error != nil {
		return "", nil, "", errors.New(data.Error.Info)
	}
	if len(data.Query.Pages) == 0 || len(data.Query.Pages[0].ImageInfo) == 0 {
		return "", nil, "", errors.New("thumbnail not found")
	}
	info := data.Query.Pages[0].ImageInfo[0]
	u, err := url.Parse(info.ThumbURL)
	if err != nil || u.Scheme != "https" || (u.Hostname() != "upload.wikimedia.org" && u.Hostname() != "thumb.wikimedia.org") {
		return "", nil, "", errors.New("invalid Commons thumbnail URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, "", err
	}
	req.Header.Set("User-Agent", "ArambyeolMenuBot/1.0 (menu enrichment; Go HTTP client)")
	// Refuse redirects: only Commons' canonical image host is allowed.
	client := *p.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests && p.store != nil {
			_, err := p.store.db.ExecContext(ctx, `INSERT INTO cafeteria_source_cooldowns(host,until) VALUES('commons.wikimedia.org',?) ON CONFLICT(host) DO UPDATE SET until=max(until,excluded.until)`, time.Now().Add(10*time.Minute).Unix())
			if err != nil {
				return "", nil, "", fmt.Errorf("persist thumbnail cooldown: %w", err)
			}
		}
		return "", nil, "", fmt.Errorf("thumbnail HTTP %d", resp.StatusCode)
	}
	image, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return "", nil, "", err
	}
	if len(image) == 0 || len(image) > 2<<20 {
		return "", nil, "", errors.New("invalid thumbnail size")
	}
	mime := http.DetectContentType(image)
	if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" {
		return "", nil, "", fmt.Errorf("unsupported thumbnail type %s", mime)
	}
	return u.String(), image, mime, nil
}

var pilotReportTemplate = template.Must(template.New("report").Parse(`<!doctype html><html lang="ru"><meta charset="utf-8"><title>Пилот: проверка фотографий</title><style>body{font:16px sans-serif;max-width:960px;margin:2em auto;padding:0 1em}article{border:1px solid #ddd;padding:1em;margin:1em 0;overflow:auto}img{max-width:320px;max-height:240px;float:left;margin:0 1em 1em 0}</style><h1>Пилот: проверка фотографий</h1><p>Решения модели не опубликованы. Всегда проверяйте их вручную.</p><p>Учтено: ${{printf "%.6f" .Cost}}; зарезервировано: ${{printf "%.2f" .Reserved}}</p>{{range .Candidates}}<article>{{if .Thumb}}<img src="{{.Thumb}}" alt="Кандидат">{{end}}<h2>{{.Translation}} ({{.Name}})</h2><p><strong>{{.Decision}}</strong> — {{.Reason}}</p><p>Статус: {{.State}}; модель: {{.Model}}</p><p>{{.Caption}} — <a href="{{.Source}}">источник</a> · <a href="{{.LicenseURL}}">{{.License}}</a></p><p>Автор: {{.Author}}</p></article>{{end}}</html>`))

func writeImagePilotReport(ctx context.Context, store *SQLiteStore, path string) error {
	rows, err := store.db.QueryContext(ctx, `SELECT d.name,COALESCE(c.translated_name,''),p.caption,p.source_url,p.author,p.license,p.license_url,p.thumbnail_url,p.state,p.decision,p.reason,p.model,p.cost_usd,p.reserved_usd FROM cafeteria_image_pilot p JOIN cafeteria_dishes d ON d.id=p.dish_id LEFT JOIN cafeteria_cards c ON c.dish_id=d.id WHERE p.prompt_version=? ORDER BY p.id`, pilotVersion)
	if err != nil {
		return err
	}
	defer rows.Close()
	data := struct {
		Candidates     []pilotCandidate
		Cost, Reserved float64
	}{}
	for rows.Next() {
		var c pilotCandidate
		var reserved float64
		if err := rows.Scan(&c.Name, &c.Translation, &c.Caption, &c.Source, &c.Author, &c.License, &c.LicenseURL, &c.Thumb, &c.State, &c.Decision, &c.Reason, &c.Model, &c.Cost, &reserved); err != nil {
			return err
		}
		if c.Cost.Valid {
			data.Cost += c.Cost.Float64
		} else {
			data.Reserved += reserved
		}
		data.Candidates = append(data.Candidates, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return pilotReportTemplate.Execute(f, data)
}
