package gocam

import "strings"

// The media controller graph of a camera pipeline, as the V4L2 backend sees
// it. On SoC cameras (PinePhone and similar) the capture node is the end of
// a chain sensor -> (bridge/CSI) -> video node: a sensor must be linked in
// and the formats along the chain must match before streaming can start.
// This file has no build tag so the planning can be tested anywhere.

// Media entity functions (linux/media.h).
const mediaEntFCamSensor = 0x00020001

// Media link flags.
const (
	mediaLnkFlEnabled   = 1
	mediaLnkFlImmutable = 2
)

type mediaEntity struct {
	id           uint32
	name         string
	function     uint32
	major, minor uint32 // device node of the entity (0:0 if none)
}

type mediaLink struct {
	srcEntity uint32
	srcPad    uint16
	sinkEnt   uint32
	sinkPad   uint16
	flags     uint32
}

func (l mediaLink) enabled() bool   { return l.flags&mediaLnkFlEnabled != 0 }
func (l mediaLink) immutable() bool { return l.flags&mediaLnkFlImmutable != 0 }

// mediaPlan says how to route a sensor to the capture node.
type mediaPlan struct {
	sensor  mediaEntity
	path    []mediaLink // from the sensor to the capture node
	enable  []mediaLink // links on the path to enable
	disable []mediaLink // other enabled links into the same sinks
}

// frontSensorHints are sensor names that are usually the front (selfie)
// camera, preferred for calls: the PinePhone's front camera is a GC2145.
var frontSensorHints = []string{"gc2145", "front"}

// planMediaRoute picks a sensor and the links that connect it to the
// capture entity video. A sensor already connected by enabled links wins;
// otherwise prefer is matched against sensor names (e.g. GOCAM_SENSOR),
// then the front sensor hints, then the first sensor found.
func planMediaRoute(entities []mediaEntity, links []mediaLink, video uint32, prefer string) (mediaPlan, bool) {
	byID := make(map[uint32]mediaEntity, len(entities))
	var sensors []mediaEntity
	for _, e := range entities {
		byID[e.id] = e
		if e.function == mediaEntFCamSensor {
			sensors = append(sensors, e)
		}
	}
	if len(sensors) == 0 {
		return mediaPlan{}, false
	}

	type candidate struct {
		sensor  mediaEntity
		path    []mediaLink
		enabled bool
	}
	var cands []candidate
	for _, s := range sensors {
		if path := findPath(links, s.id, video, byID); path != nil {
			en := true
			for _, l := range path {
				en = en && l.enabled()
			}
			cands = append(cands, candidate{s, path, en})
		}
	}
	if len(cands) == 0 {
		return mediaPlan{}, false
	}

	pick := -1
	matches := func(name, hint string) bool {
		return hint != "" && strings.Contains(strings.ToLower(name), strings.ToLower(hint))
	}
	if prefer != "" {
		for i, c := range cands {
			if matches(c.sensor.name, prefer) {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		for i, c := range cands {
			if c.enabled {
				pick = i
				break
			}
		}
	}
	for _, hint := range frontSensorHints {
		if pick >= 0 {
			break
		}
		for i, c := range cands {
			if matches(c.sensor.name, hint) {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		pick = 0
	}

	c := cands[pick]
	plan := mediaPlan{sensor: c.sensor, path: c.path}
	onPath := make(map[mediaLink]bool, len(c.path))
	for _, l := range c.path {
		onPath[l] = true
		if !l.enabled() && !l.immutable() {
			plan.enable = append(plan.enable, l)
		}
	}
	// A sink pad takes one source at a time: unlink the other sources
	// (another sensor) that feed the same pads.
	for _, l := range links {
		if onPath[l] || !l.enabled() || l.immutable() {
			continue
		}
		for _, p := range c.path {
			if l.sinkEnt == p.sinkEnt && l.sinkPad == p.sinkPad {
				plan.disable = append(plan.disable, l)
				break
			}
		}
	}
	return plan, true
}

// findPath finds links from entity from to entity to (breadth first,
// through any link, enabled or not, not passing through other sensors).
func findPath(links []mediaLink, from, to uint32, byID map[uint32]mediaEntity) []mediaLink {
	prev := map[uint32]mediaLink{}
	seen := map[uint32]bool{from: true}
	queue := []uint32{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == to {
			var path []mediaLink
			for n := to; n != from; {
				l := prev[n]
				path = append([]mediaLink{l}, path...)
				n = l.srcEntity
			}
			return path
		}
		for _, l := range links {
			if l.srcEntity != cur || seen[l.sinkEnt] {
				continue
			}
			if e, ok := byID[l.sinkEnt]; ok && e.function == mediaEntFCamSensor {
				continue
			}
			seen[l.sinkEnt] = true
			prev[l.sinkEnt] = l
			queue = append(queue, l.sinkEnt)
		}
	}
	return nil
}
