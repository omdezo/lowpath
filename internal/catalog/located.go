package catalog

import (
	"fmt"
	"math"
	"time"
)

// fibreKMPerMS is how far light gets in optical fibre in a millisecond:
// c / 1.468 (the group index of standard single-mode fibre) ≈ 204 km/ms.
// Nothing on the internet is faster, so a round trip of r ms puts the
// server within r/2 × 204 km of this PC, whatever a database says.
const fibreKMPerMS = 204.0

// geoSlackKM allows for the error of city-level IP geolocation at both
// ends: GeoLite places an address at its city's center or its ISP's
// registered city, often tens and sometimes a few hundred kilometers off.
const geoSlackKM = 250

// Geo check states.
const (
	GeoCheckOK      = "ok"      // the city is reachable in the measured ping
	GeoCheckTooFar  = "too_far" // the city is farther than light gets in that time: the database is wrong
	GeoCheckUnknown = "unknown" // no ping, or no city for one of the ends
)

// GeoCheck says whether an IP database's city for a server can be right,
// given the fastest ping measured to it.
type GeoCheck struct {
	State      string   `json:"state"`
	DistanceKM *float64 `json:"distance_km"` // this PC's city to the server's city
	MaxKM      *float64 `json:"max_km"`      // the farthest the server can be for that ping
	Note       string   `json:"note,omitempty"`
}

// CheckGeo compares the distance between you and server (both known city
// coordinates) with how far light in fibre travels in half the fastest
// round trip, minRTT (0 = not measured). A city that's too far means the
// geolocation is wrong (common for anycast and for addresses registered
// at a company's head office), and the note says what the ping does
// prove.
func CheckGeo(you, server Geo, minRTT time.Duration) GeoCheck {
	var c GeoCheck
	if you.valid() && server.valid() {
		d := DistanceKM(you, server)
		c.DistanceKM = &d
	}
	if minRTT > 0 {
		m := minRTT.Seconds() * 1000 / 2 * fibreKMPerMS
		c.MaxKM = &m
	}
	switch {
	case c.MaxKM == nil:
		// No reply yet, none ever, or no way to ping: the ping note says
		// which, so this one doesn't guess.
		c.State, c.Note = GeoCheckUnknown, "Not checked yet: it needs a ping reply."
	case c.DistanceKM == nil:
		c.State, c.Note = GeoCheckUnknown, fmt.Sprintf("The ping puts it within %s of you.", km(*c.MaxKM))
	case *c.DistanceKM > *c.MaxKM+geoSlackKM:
		c.State = GeoCheckTooFar
		c.Note = fmt.Sprintf("Can't be right: that city is %s from where the IP database puts you, but the ping puts the server within %s of you.", km(*c.DistanceKM), km(*c.MaxKM))
	default:
		c.State, c.Note = GeoCheckOK, fmt.Sprintf("Matches the ping: %s away, the ping allows up to %s.", km(*c.DistanceKM), km(*c.MaxKM))
	}
	return c
}

// km rounds a distance for display: to 10 km under 1000 km, else to 100.
func km(v float64) string {
	switch {
	case v < 100:
		return fmt.Sprintf("%.0f km", math.Max(v, 0))
	case v < 1000:
		return fmt.Sprintf("%.0f km", math.Round(v/10)*10)
	default:
		return fmt.Sprintf("%.0f km", math.Round(v/100)*100)
	}
}

// NewGeo makes a known point from latitude and longitude, or an unknown one
// when they're out of range.
func NewGeo(lat, lon float64) Geo {
	g := Geo{Lat: lat, Lon: lon, Known: finite(lat) && finite(lon)}
	if !g.valid() {
		return Geo{}
	}
	return g
}
