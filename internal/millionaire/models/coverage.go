package models

// BandCoverage is one row of the M6 per-band coverage report (cmd/label
// -coverage): the number of rated questions whose rank falls in quantile
// band L and how many of those are cultural (taste-layer share).
//
// band is 1..15. total/cultural_total are raw counts; the caller derives
// the cultural percentage.
type BandCoverage struct {
	Band          int `ksql:"band" json:"band"`
	Total         int `ksql:"total" json:"total"`
	CulturalTotal int `ksql:"cultural_total" json:"cultural_total"`
}
