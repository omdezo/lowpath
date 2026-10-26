package catalog

import (
	"math"
	"strings"
)

// Geo is a point on Earth. Known is false when there are no coordinates
// worth showing.
type Geo struct {
	Lat, Lon float64
	Known    bool
}

// GeoFix says what Sanitize did to a POP's coordinates.
type GeoFix string

const (
	GeoOK        GeoFix = ""          // Valve's coordinates were right
	GeoSwapped   GeoFix = "swapped"   // latitude and longitude were the wrong way round
	GeoCorrected GeoFix = "corrected" // wrong (a flipped sign, another city); replaced by the city's
	GeoUnknown   GeoFix = "unknown"   // unusable and nothing to replace them with
)

// geoTolerance is how far (km) a POP may sit from the center of the city
// its name gives: data centers are in the suburbs, a wrong point is
// hundreds of kilometers off.
const geoTolerance = 300

// rawGeo reads Valve's [longitude, latitude] pair.
func rawGeo(v []float64) Geo {
	if len(v) != 2 || !finite(v[0]) || !finite(v[1]) || (v[0] == 0 && v[1] == 0) {
		return Geo{}
	}
	return Geo{Lon: v[0], Lat: v[1], Known: true}
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func (g Geo) valid() bool {
	return g.Known && g.Lat >= -90 && g.Lat <= 90 && g.Lon >= -180 && g.Lon <= 180
}

func (g Geo) swapped() Geo { return Geo{Lat: g.Lon, Lon: g.Lat, Known: g.Known} }

// DistanceKM is the great-circle distance between two known points.
func DistanceKM(a, b Geo) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dLat := (b.Lat - a.Lat) * rad
	dLon := (b.Lon - a.Lon) * rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

// Sanitize fixes the coordinates Valve's config gets wrong. Seen in the
// live config (October 2026): Datapacket Frankfurt, Vienna and London have
// latitude and longitude swapped, Madrid has the sign of its longitude
// flipped (it lands in the Mediterranean east of Valencia), and Datapacket
// Stockholm sits on Moscow. Each POP is checked against the city its id and
// name give (a small table of metro coordinates), or failing that against
// another POP in the same city; with neither, only impossible values are
// fixed.
func Sanitize(pops []POP) {
	refs := make([]Geo, len(pops))
	for i := range pops {
		refs[i] = metroOf(&pops[i])
	}
	// POPs with no table entry borrow the reference of a POP in the same
	// city ("Multiplay Dubai" from "Dubai").
	for i := range pops {
		if refs[i].Known {
			continue
		}
		c := cityKey(pops[i].City)
		for j := range pops {
			if j != i && refs[j].Known && c != "" && cityKey(pops[j].City) == c {
				refs[i] = refs[j]
				break
			}
		}
	}
	for i := range pops {
		pops[i].Geo, pops[i].GeoFix = fixGeo(pops[i].Geo, refs[i])
	}
}

// fixGeo checks g against the reference point ref (when known).
func fixGeo(g, ref Geo) (Geo, GeoFix) {
	if ref.Known {
		switch {
		case g.valid() && DistanceKM(g, ref) <= geoTolerance:
			return g, GeoOK
		case g.swapped().valid() && DistanceKM(g.swapped(), ref) <= geoTolerance:
			return g.swapped(), GeoSwapped
		default:
			return ref, GeoCorrected
		}
	}
	switch {
	case g.valid():
		return g, GeoOK
	case g.swapped().valid():
		// A longitude can't be a latitude above 90, so this pair can only
		// have been written the wrong way round.
		return g.swapped(), GeoSwapped
	default:
		return Geo{}, GeoUnknown
	}
}

// cityKey reduces a POP's city to the place itself: "Datapacket Frankfurt"
// and "Frankfurt" are the same city, as are "Stockholm - Kista" and
// "Stockholm - Bromma".
func cityKey(city string) string {
	c := strings.ToLower(city)
	if i := strings.Index(c, " - "); i >= 0 {
		c = c[:i]
	}
	for _, p := range []string{"datapacket ", "multiplay ", "alibaba cloud ", "tencent ", "hetzner "} {
		c = strings.TrimPrefix(c, p)
	}
	return strings.TrimSpace(c)
}

// metroOf finds the reference point for a POP: its id, an alias, or the id
// without the parts Valve adds for a second site or a partner ("bom2",
// "ctum"/"ctut"/"ctuu" for Chinese carriers, "dfra" for Datapacket). The
// metro only counts when its city appears in the POP's name, so a reused
// or renamed id can't pull a POP to the wrong place.
func metroOf(p *POP) Geo {
	name := strings.ToLower(p.Desc)
	for _, code := range metroCodes(p) {
		if m, ok := metro(code); ok && strings.Contains(name, strings.ToLower(m.city)) {
			return Geo{Lat: m.lat, Lon: m.lon, Known: true}
		}
	}
	return Geo{}
}

func metroCodes(p *POP) []string {
	codes := append([]string{p.ID}, p.Aliases...)
	for _, id := range append([]string{p.ID}, p.Aliases...) {
		base := strings.TrimRight(id, "0123456789")
		if base != id && len(base) >= 3 {
			codes = append(codes, base)
		}
		if len(base) == 4 {
			if strings.ContainsRune("mtu", rune(base[3])) {
				codes = append(codes, base[:3])
			}
			if base[0] == 'd' {
				codes = append(codes, base[1:])
			}
		}
	}
	return codes
}

type metroRef struct {
	city     string
	lat, lon float64
}

// metro gives the city center for a Valve POP metro code (mostly IATA
// codes). The list covers every POP in the October 2026 config; POPs it
// does not know are still checked against the others.
func metro(code string) (metroRef, bool) {
	m := func(city string, lat, lon float64) (metroRef, bool) { return metroRef{city, lat, lon}, true }
	switch code {
	case "ams":
		return m("Amsterdam", 52.37, 4.90)
	case "atl":
		return m("Atlanta", 33.75, -84.39)
	case "bom":
		return m("Mumbai", 19.08, 72.88)
	case "ctu":
		return m("Chengdu", 30.66, 104.07)
	case "dfw":
		return m("Dallas", 32.78, -96.80)
	case "dxb":
		return m("Dubai", 25.20, 55.27)
	case "eat":
		return m("Wenatchee", 47.42, -120.31)
	case "eze":
		return m("Buenos Aires", -34.60, -58.38)
	case "fjr":
		return m("Fujairah", 25.13, 56.33)
	case "fra":
		return m("Frankfurt", 50.11, 8.68)
	case "fsn":
		return m("Falkenstein", 50.48, 12.37)
	case "gru":
		return m("Sao Paulo", -23.55, -46.63)
	case "gum":
		return m("Guam", 13.44, 144.79)
	case "hel":
		return m("Helsinki", 60.17, 24.94)
	case "hkg":
		return m("Hong Kong", 22.32, 114.17)
	case "iad":
		return m("Sterling", 39.01, -77.43)
	case "jnb":
		return m("Johannesburg", -26.20, 28.05)
	case "lax":
		return m("Los Angeles", 34.05, -118.24)
	case "lhr":
		return m("London", 51.51, -0.13)
	case "lim":
		return m("Lima", -12.05, -77.04)
	case "maa":
		return m("Chennai", 13.08, 80.27)
	case "mad":
		return m("Madrid", 40.42, -3.70)
	case "ord":
		return m("Chicago", 41.88, -87.63)
	case "par":
		return m("Paris", 48.86, 2.35)
	case "pek":
		return m("Beijing", 39.90, 116.41)
	case "pvg":
		return m("Shanghai", 31.23, 121.47)
	case "scl":
		return m("Santiago", -33.45, -70.67)
	case "sea":
		return m("Seattle", 47.61, -122.33)
	case "seo":
		return m("Seoul", 37.57, 126.98)
	case "sgp":
		return m("Singapore", 1.29, 103.85)
	case "sto":
		return m("Stockholm", 59.33, 18.07)
	case "syd":
		return m("Sydney", -33.87, 151.21)
	case "tgd":
		return m("Guangzhou", 23.13, 113.26)
	case "tyo":
		return m("Tokyo", 35.68, 139.69)
	case "vie":
		return m("Vienna", 48.21, 16.37)
	case "waw":
		return m("Warsaw", 52.23, 21.01)
	}
	return metroRef{}, false
}
