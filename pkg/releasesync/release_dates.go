package releasesync

import "time"

var GADateMap = map[string]time.Time{
	"4.13": time.Date(2023, 5, 17, 0, 0, 0, 0, time.UTC),
	"4.12": time.Date(2023, 1, 17, 0, 0, 0, 0, time.UTC),
	"4.11": time.Date(2022, 8, 10, 0, 0, 0, 0, time.UTC),
	"4.10": time.Date(2022, 3, 10, 0, 0, 0, 0, time.UTC),
	"4.9":  time.Date(2021, 10, 18, 0, 0, 0, 0, time.UTC),
	"4.8":  time.Date(2021, 7, 27, 0, 0, 0, 0, time.UTC),
	"4.7":  time.Date(2021, 2, 24, 0, 0, 0, 0, time.UTC),
	"4.6":  time.Date(2020, 10, 27, 0, 0, 0, 0, time.UTC),
}
