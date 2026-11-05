package gocam

import "testing"

// PinePhone (Allwinner A64), older kernels: both sensors link straight to
// the CSI capture node; one link may be enabled at a time.
func pinephoneOld(ovEnabled, gcEnabled bool) ([]mediaEntity, []mediaLink) {
	ents := []mediaEntity{
		{id: 1, name: "sun6i-csi", major: 81, minor: 1},
		{id: 4, name: "ov5640 3-004c", function: mediaEntFCamSensor, major: 81, minor: 2},
		{id: 6, name: "gc2145 3-003c", function: mediaEntFCamSensor, major: 81, minor: 3},
	}
	fl := func(on bool) uint32 {
		if on {
			return mediaLnkFlEnabled
		}
		return 0
	}
	links := []mediaLink{
		{srcEntity: 4, srcPad: 0, sinkEnt: 1, sinkPad: 0, flags: fl(ovEnabled)},
		{srcEntity: 6, srcPad: 0, sinkEnt: 1, sinkPad: 0, flags: fl(gcEnabled)},
	}
	return ents, links
}

func TestPlanPrefersFrontSensor(t *testing.T) {
	ents, links := pinephoneOld(false, false)
	plan, ok := planMediaRoute(ents, links, 1, "")
	if !ok || plan.sensor.id != 6 {
		t.Fatalf("picked %+v, want the gc2145 (front)", plan.sensor)
	}
	if len(plan.enable) != 1 || plan.enable[0].srcEntity != 6 || len(plan.disable) != 0 {
		t.Fatalf("plan %+v", plan)
	}
}

func TestPlanKeepsConnectedSensor(t *testing.T) {
	ents, links := pinephoneOld(true, false)
	plan, ok := planMediaRoute(ents, links, 1, "")
	if !ok || plan.sensor.id != 4 || len(plan.enable) != 0 || len(plan.disable) != 0 {
		t.Fatalf("an already linked sensor must be used as is: %+v", plan)
	}
}

func TestPlanSwitchesSensorOnRequest(t *testing.T) {
	ents, links := pinephoneOld(true, false)
	plan, ok := planMediaRoute(ents, links, 1, "GC2145")
	if !ok || plan.sensor.id != 6 {
		t.Fatalf("GOCAM_SENSOR=GC2145 picked %+v", plan.sensor)
	}
	if len(plan.disable) != 1 || plan.disable[0].srcEntity != 4 || len(plan.enable) != 1 {
		t.Fatalf("the ov5640 link must be dropped and the gc2145 one enabled: %+v", plan)
	}
}

// Newer kernels: sensors -> sun6i-csi-bridge (sub-device) -> capture node.
func TestPlanThroughBridge(t *testing.T) {
	ents := []mediaEntity{
		{id: 1, name: "sun6i-csi-bridge", major: 81, minor: 5},
		{id: 4, name: "sun6i-csi-capture", major: 81, minor: 1},
		{id: 10, name: "ov5640 3-004c", function: mediaEntFCamSensor, major: 81, minor: 2},
		{id: 12, name: "gc2145 3-003c", function: mediaEntFCamSensor, major: 81, minor: 3},
	}
	links := []mediaLink{
		{srcEntity: 1, srcPad: 1, sinkEnt: 4, sinkPad: 0, flags: mediaLnkFlEnabled | mediaLnkFlImmutable},
		{srcEntity: 10, srcPad: 0, sinkEnt: 1, sinkPad: 0},
		{srcEntity: 12, srcPad: 0, sinkEnt: 1, sinkPad: 0},
	}
	plan, ok := planMediaRoute(ents, links, 4, "")
	if !ok || plan.sensor.id != 12 || len(plan.path) != 2 {
		t.Fatalf("plan %+v", plan)
	}
	if plan.path[0].sinkEnt != 1 || plan.path[1].sinkEnt != 4 {
		t.Fatalf("path %+v", plan.path)
	}
	if len(plan.enable) != 1 || plan.enable[0].srcEntity != 12 {
		t.Fatalf("only the sensor link must be enabled (the bridge link is immutable): %+v", plan.enable)
	}
}

func TestPlanNoSensor(t *testing.T) {
	ents := []mediaEntity{{id: 1, name: "uvcvideo"}}
	if _, ok := planMediaRoute(ents, nil, 1, ""); ok {
		t.Fatal("a USB webcam without sensor entities has no route")
	}
}
