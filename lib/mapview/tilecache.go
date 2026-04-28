package mapview

import (
	"fmt"
	"image"
	"image/draw"
	_ "image/png"
	"net/http"
	"sync"
	"time"
)

// tileKey identifies a Web Mercator slippy-map tile.
type tileKey struct{ z, x, y int }

// tileCache is an in-memory store for downloaded map tiles.
// On a cache miss, a background goroutine fetches the tile and calls notify
// so the map widget can redraw once it arrives.
type tileCache struct {
	mu      sync.Mutex
	tiles   map[tileKey]*image.RGBA
	pending map[tileKey]bool
	notify  func()
	client  *http.Client
	sub     int // cycles 0-3 across CartoDB subdomains
}

func newTileCache(notify func()) *tileCache {
	return &tileCache{
		tiles:   make(map[tileKey]*image.RGBA),
		pending: make(map[tileKey]bool),
		notify:  notify,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// get returns the cached tile, or nil if not yet available.
// On a miss it queues a background fetch; notify is called when the tile arrives.
func (c *tileCache) get(z, x, y int) *image.RGBA {
	k := tileKey{z, x, y}
	c.mu.Lock()
	defer c.mu.Unlock()
	if img, ok := c.tiles[k]; ok {
		return img
	}
	if !c.pending[k] {
		c.pending[k] = true
		go c.fetch(k)
	}
	return nil
}

// CartoDB Dark Matter — dark basemap, no API key required.
const tileURLFmt = "https://%s.basemaps.cartocdn.com/dark_all/%d/%d/%d.png"

var tileSubdomains = []string{"a", "b", "c", "d"}

func (c *tileCache) fetch(k tileKey) {
	c.mu.Lock()
	sub := tileSubdomains[c.sub%4]
	c.sub++
	c.mu.Unlock()

	url := fmt.Sprintf(tileURLFmt, sub, k.z, k.x, k.y)
	resp, err := c.client.Get(url) //nolint:noctx
	if err != nil {
		c.mu.Lock()
		delete(c.pending, k)
		c.mu.Unlock()
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.mu.Lock()
		delete(c.pending, k)
		c.mu.Unlock()
		return
	}

	src, _, err := image.Decode(resp.Body)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, k)
		c.mu.Unlock()
		return
	}

	rgba := image.NewRGBA(src.Bounds())
	draw.Draw(rgba, rgba.Bounds(), src, image.Point{}, draw.Src)

	c.mu.Lock()
	// Evict a random entry when the cache exceeds 512 tiles (~32 MB at 256×256×4 B each).
	for len(c.tiles) >= 512 {
		for victim := range c.tiles {
			delete(c.tiles, victim)
			break
		}
	}
	c.tiles[k] = rgba
	delete(c.pending, k)
	c.mu.Unlock()

	if c.notify != nil {
		c.notify()
	}
}
