package discover

import (
	"context"
	"errors"
	"time"
)

const catalogRecommendationLimit = 500
const catalogRecommendationHotReason = "完整片库热门 · 按热度排序"
const catalogRecommendationNewReason = "完整片库近期上线 · 按首播或上映日期排序"

// catalogRecommendations keeps the homepage current when its Douban source is
// unreachable. It reads a bounded pool of known works from the persistent
// directory; metadata requests and a complete directory scan are unnecessary.
func (s *Service) catalogRecommendations(ctx context.Context, category, collection string, now time.Time) ([]Item, error) {
	if !contains([]string{"movie", "tv", "animation", "variety", "documentary"}, category) || !contains([]string{"hot", "new"}, collection) {
		return nil, errors.New("catalog recommendation selection unavailable")
	}
	store, err := s.openFullCatalog()
	if err != nil {
		return nil, err
	}

	// The category projection excludes removed, matched duplicates and future
	// releases. Its popularity index allows hot recommendations to stop at the
	// requested limit rather than sorting the entire category.
	query := "SELECT " + fullItemColumns + ` FROM visible_categories v INDEXED BY visible_category_order
 JOIN entries e ON e.key=v.key
 WHERE v.category=? AND (e.hydrated=1 OR e.origin='douban') AND v.popularity>0
 ORDER BY v.popularity DESC,v.key LIMIT ?`
	args := []any{category, catalogRecommendationLimit}
	reason := catalogRecommendationHotReason
	if collection == "new" {
		// Leading with the release-date index bounds the sort to the recent
		// interval. Unknown dates never become newly released recommendations.
		query = "SELECT " + fullItemColumns + ` FROM entries e INDEXED BY entries_release_date
 CROSS JOIN visible_categories v ON v.key=e.key
 WHERE e.release_date>=? AND e.release_date<=? AND v.category=?
 AND (e.hydrated=1 OR e.origin='douban')
 ORDER BY e.release_date DESC,e.popularity DESC,e.key LIMIT ?`
		args = []any{now.UTC().AddDate(0, 0, -90).Format("2006-01-02"), now.UTC().Format("2006-01-02"), category, catalogRecommendationLimit}
		reason = catalogRecommendationNewReason
	}
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.New("catalog recommendations unavailable")
	}
	defer rows.Close()
	items := make([]Item, 0, catalogRecommendationLimit)
	for rows.Next() {
		item, scanErr := scanFullItem(rows)
		if scanErr != nil {
			return nil, errors.New("catalog recommendations unavailable")
		}
		if collection == "new" {
			if _, dateErr := time.Parse("2006-01-02", item.ReleaseDate); dateErr != nil {
				continue
			}
		}
		item.Reason = reason
		items = append(items, item)
	}
	if rows.Err() != nil || len(items) == 0 {
		return nil, errors.New("catalog recommendations unavailable")
	}
	return items, nil
}

// Directory pools already contain their complete bounded selection. Appending
// old source memberships would reintroduce stale or undated "new" releases.
func isCatalogRecommendation(items []Item) bool {
	if len(items) == 0 || (items[0].Reason != catalogRecommendationHotReason && items[0].Reason != catalogRecommendationNewReason) {
		return false
	}
	for _, item := range items[1:] {
		if item.Reason != items[0].Reason {
			return false
		}
	}
	return true
}
