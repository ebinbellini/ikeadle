# IKEAdle

Guess the price of products from [ikea.se](https://www.ikea.com/se/sv/).

IKEAdle is a price-guessing game in IKEA's visual language: a white IKEA-style
header, blue and yellow accents, Noto Sans, flat cards and a yellow price tag.
It is an unofficial fan project, not affiliated with IKEA.

## Requirements

- Go 1.27 or newer

## Configure

Copy `.env.example` to `.env` and edit the values therein.

## Run

```bash
go run .
```

Then open <http://localhost:2760>.

## Game modes

- Dagens vara: one IKEA product per day.
- Jag har tur: a random product with six guesses.
- Kund mot kund: compete in a lobby of 2-5 players.

## How the scraper works

IKEA's product listing is rendered from Ingka's **SIK search API**
(`sik.search.blue.cdtapps.com`), which is also what ikea.com calls from the
browser. Scraping that JSON API is much faster and more robust than parsing the
HTML pages:

1. `POST /se/sv/search` with `input=products`, `type=CATEGORY` and an
   `offset`/`size` window reads the whole Swedish range in one paginated
   query. One response contains up to 500 products with name, type, design,
   measurements, price, image, product URL and the full IKEA category path.
2. If the root category cannot be read, the scraper falls back to
   `GET /se/sv/search-result-page?size=1`. Its `f-subcategories` filter lists
   the top-level categories, so no category ids are hard-coded, and each of
   those categories is paginated separately.
3. The scraper follows `metadata.max` until the catalogue is exhausted,
   deduplicates by IKEA's `itemNo`, and stores everything in one gob cache
   file.

The complete Swedish range is around 10 000 unique products, which takes about
twenty requests instead of one request per product or per HTML page. Items
without a price, an image or a name are skipped.

### Politeness

- `https://sik.search.blue.cdtapps.com/robots.txt` is `User-agent: *` /
  `Allow: /`; there is no crawl delay or visit window.
- IKEAdle still waits `SCRAPE_DELAY` (default 1 s) between requests and allows
  only one scrape at a time.
- A full scrape runs weekly (`SCRAPE_CRON_SCHED`, Monday 06:00 by default) and
  on startup only when there is no usable cache.
- The request carries a descriptive User-Agent.

If IKEA changes the API contract version, set `IKEA_API_VERSION` in `.env`
instead of rebuilding. The built-in default is `20260727`.

## Development

- `DEV_MODE=true` serves `web_static/` directly from disk.
- `web_static/icons/full-logo.svg` is the header wordmark (text converted to
  paths, so it renders the same without the original font).
- `utils/generate-favicons.sh` rebuilds the favicon and app icon set from
  `utils/IKEAdle favicon.svg`.
- `go test ./...` runs the server and scraper tests.

IKEA® is a trademark of Inter IKEA Systems B.V. This project is not affiliated
with or endorsed by IKEA.
