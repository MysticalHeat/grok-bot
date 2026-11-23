package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTrayDecisionRotation(t *testing.T) {
	for _, rotation := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprint(rotation), func(t *testing.T) {
			store := testStore(t)
			now := time.Now()
			schedule := map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "Rice"}}}
			if err := store.saveCafeteriaWeek(context.Background(), []CafeteriaWeeklyPlan{
				{Date: "2026-11-21", MenusByMealType: schedule},
			}, now); err != nil {
				t.Fatal(err)
			}
			p := &trayPhotos{store: store, frames: t.TempDir(), now: func() time.Time { return now }, admins: map[int64]bool{123: true}}
			handler := newMenuHandler(store, "token", p)
			req := func(method, path string, body []byte, want int) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(method, path, bytes.NewReader(body))
				r.Header.Set("X-Telegram-Init-Data", signedInitDataForUser("token", now, 123))
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body)
				}
				return w
			}
			src := image.NewRGBA(image.Rect(0, 0, 120, 80))
			for y := 0; y < 80; y++ {
				for x := 0; x < 120; x++ {
					src.SetRGBA(x, y, color.RGBA{uint8(x * 2), uint8(y * 3), uint8((x + y) % 180), 255})
				}
			}
			var upload bytes.Buffer
			if err := png.Encode(&upload, src); err != nil {
				t.Fatal(err)
			}
			req("POST", "/api/tray?date=2026-11-21&meal=LUNCH", upload.Bytes(), 201)
			before := bytes.Clone(req("GET", "/api/tray/1/frame", nil, 200).Body.Bytes())
			for _, invalid := range []string{"-1", "4", "1.5", `"1"`, "null", "true"} {
				body := fmt.Appendf(nil, `{"crops":[],"rotation":%s}`, invalid)
				req("POST", "/api/tray/1/decision", body, 400)
			}
			rotationField := fmt.Sprintf(`,"rotation":%d`, rotation)
			if rotation == 0 {
				rotationField = "" // Existing clients omit rotation.
			}
			body := fmt.Appendf(nil, `{"crops":[{"dish_id":1,"x":250,"y":125,"w":500,"h":500}]%s}`, rotationField)
			req("POST", "/api/tray/1/decision", body, 204)
			after := req("GET", "/api/tray/1/frame", nil, 200).Body.Bytes()
			if !bytes.Equal(before, after) {
				t.Fatal("original frame changed")
			}
			var published []byte
			if err := store.db.QueryRow(`SELECT jpeg FROM tray_published WHERE dish_id=1`).Scan(&published); err != nil {
				t.Fatal(err)
			}
			original, err := jpeg.Decode(bytes.NewReader(before))
			if err != nil {
				t.Fatal(err)
			}
			got, err := jpeg.Decode(bytes.NewReader(published))
			if err != nil {
				t.Fatal(err)
			}
			w, h := 120, 80
			if rotation%2 == 1 {
				w, h = h, w
			}
			x0, y0 := w*250/1000, h*125/1000
			x1, y1 := (w*750+999)/1000, (h*625+999)/1000
			if got.Bounds().Dx() != x1-x0 || got.Bounds().Dy() != y1-y0 {
				t.Fatalf("crop dimensions %v, want %dx%d", got.Bounds(), x1-x0, y1-y0)
			}
			for _, point := range []image.Point{{5, 5}, {x1 - x0 - 6, 5}, {5, y1 - y0 - 6}, {x1 - x0 - 6, y1 - y0 - 6}} {
				x, y := x0+point.X, y0+point.Y
				switch rotation {
				case 1:
					x, y = y, 79-x
				case 2:
					x, y = 119-x, 79-y
				case 3:
					x, y = 119-y, x
				}
				r1, g1, b1, _ := original.At(x, y).RGBA()
				r2, g2, b2, _ := got.At(point.X, point.Y).RGBA()
				for _, pair := range [][2]uint32{{r1, r2}, {g1, g2}, {b1, b2}} {
					if absTrayDifference(pair[0], pair[1]) > 20*257 {
						t.Fatalf("at %v: got %v, want approximately %v", point, got.At(point.X, point.Y), original.At(x, y))
					}
				}
			}
		})
	}
}

func absTrayDifference(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}
