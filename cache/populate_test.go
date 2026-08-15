package cache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"grout/romm"
)

func TestFetchPlatformGamesPersistsEachPageBeforeFetchingNext(t *testing.T) {
	cm := newTestManager(t)
	const platformID = 42
	const total = 501

	var offsets []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/roms" {
			t.Errorf("path = %q, want /api/roms", r.URL.Path)
			http.NotFound(w, r)
			return
		}

		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		offsets = append(offsets, offset)
		if limit != DefaultRomPageSize {
			t.Errorf("limit = %d, want %d", limit, DefaultRomPageSize)
		}

		// The previous page must already be committed before the next request.
		// Retaining all pages until the end is what exhausts 128MB handhelds.
		if offset > 0 {
			var cached int
			if err := cm.db.QueryRow("SELECT COUNT(*) FROM games").Scan(&cached); err != nil {
				t.Errorf("count cached games: %v", err)
			} else if cached != offset {
				t.Errorf("before offset %d request, cached games = %d", offset, cached)
			}
		}

		end := offset + limit
		if end > total {
			end = total
		}
		items := make([]romm.Rom, 0, end-offset)
		for id := offset + 1; id <= end; id++ {
			items = append(items, romm.Rom{
				ID:             id,
				PlatformID:     platformID,
				PlatformFSSlug: "test",
				Name:           "Game " + strconv.Itoa(id),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(romm.PaginatedRoms{
			Items: items, Total: total, Limit: limit, Offset: offset,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	count, err := cm.fetchPlatformGames(romm.Platform{ID: platformID, Name: "Test"}, &fetchOpts{
		client: romm.NewClient(server.URL),
	})
	if err != nil {
		t.Fatalf("fetchPlatformGames: %v", err)
	}
	if count != total {
		t.Fatalf("count = %d, want %d", count, total)
	}

	wantOffsets := []int{0, 250, 500}
	if len(offsets) != len(wantOffsets) {
		t.Fatalf("offsets = %v, want %v", offsets, wantOffsets)
	}
	for i := range wantOffsets {
		if offsets[i] != wantOffsets[i] {
			t.Fatalf("offsets = %v, want %v", offsets, wantOffsets)
		}
	}
}
