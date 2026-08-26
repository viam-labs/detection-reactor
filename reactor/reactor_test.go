package reactor

import (
	"context"
	"errors"
	"image"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
	"go.viam.com/rdk/vision/objectdetection"
	"go.viam.com/test"
)

func validConfig() *Config {
	return &Config{
		VisionService: "gestures",
		Camera:        "camera-1",
		Target:        "drawer",
		LabelCommands: map[string]map[string]interface{}{
			"Victory": {"capture_and_draw": map[string]interface{}{}},
		},
	}
}

func det(label string, score float64) objectdetection.Detection {
	return objectdetection.NewDetectionWithoutImgBounds(image.Rect(0, 0, 10, 10), score, label)
}

func newTestReactor(t *testing.T, cfg *Config, do func(context.Context, map[string]interface{}) (map[string]interface{}, error)) *reactor {
	t.Helper()
	target := inject.NewGenericService("drawer")
	target.DoFunc = do
	if cfg.MinConfidence == 0 {
		cfg.MinConfidence = defaultMinConfidence
	}
	return &reactor{logger: logging.NewTestLogger(t), cfg: cfg, target: target}
}

func TestConfigValidate_requiredFields(t *testing.T) {
	for _, tc := range []struct {
		field  string
		mutate func(*Config)
	}{
		{"vision_service", func(c *Config) { c.VisionService = "" }},
		{"camera", func(c *Config) { c.Camera = "" }},
		{"target", func(c *Config) { c.Target = "" }},
		{"label_commands", func(c *Config) { c.LabelCommands = nil }},
	} {
		cfg := validConfig()
		tc.mutate(cfg)
		_, _, err := cfg.Validate("")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, tc.field)
	}
}

func TestConfigValidate_emptyCommandPayload(t *testing.T) {
	cfg := validConfig()
	cfg.LabelCommands = map[string]map[string]interface{}{"Victory": {}}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "Victory")
}

func TestConfigValidate_confidenceRange(t *testing.T) {
	cfg := validConfig()
	cfg.MinConfidence = 1.5
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "min_confidence")
}

func TestConfigValidate_dependencies(t *testing.T) {
	deps, _, err := validConfig().Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"gestures", "camera-1", "rdk:service:generic/drawer"})
}

func TestTargetName_bareNameIsAGenericService(t *testing.T) {
	name, err := (&Config{Target: "drawer"}).TargetName()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, name.String(), test.ShouldEqual, "rdk:service:generic/drawer")
}

func TestTargetName_qualifiedNamePassesThrough(t *testing.T) {
	name, err := (&Config{Target: "rdk:component:sensor/arm-recorder"}).TargetName()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, name.String(), test.ShouldEqual, "rdk:component:sensor/arm-recorder")
}

func TestPickLabel_highestConfidenceMappedLabel(t *testing.T) {
	cfg := validConfig()
	cfg.LabelCommands["Thumb_Up"] = map[string]interface{}{"go_home": map[string]interface{}{}}
	r := newTestReactor(t, cfg, nil)
	got := r.pickLabel([]objectdetection.Detection{det("Victory", 0.6), det("Thumb_Up", 0.9)})
	test.That(t, got, test.ShouldEqual, "Thumb_Up")
}

func TestPickLabel_ignoresUnmappedLabels(t *testing.T) {
	r := newTestReactor(t, validConfig(), nil)
	got := r.pickLabel([]objectdetection.Detection{det("Open_Palm", 0.99)})
	test.That(t, got, test.ShouldBeEmpty)
}

func TestPickLabel_ignoresLowConfidence(t *testing.T) {
	r := newTestReactor(t, validConfig(), nil)
	test.That(t, r.pickLabel([]objectdetection.Detection{det("Victory", 0.49)}), test.ShouldBeEmpty)
	test.That(t, r.pickLabel([]objectdetection.Detection{det("Victory", 0.51)}), test.ShouldEqual, "Victory")
}

func TestFire_sendsThePayloadUntouched(t *testing.T) {
	var got map[string]interface{}
	r := newTestReactor(t, validConfig(), func(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
		got = cmd
		return map[string]interface{}{"ok": true}, nil
	})
	r.fire(context.Background(), "Victory")
	test.That(t, got, test.ShouldResemble, map[string]interface{}{"capture_and_draw": map[string]interface{}{}})
	test.That(t, r.firedCount, test.ShouldEqual, 1)
	test.That(t, r.lastLabel, test.ShouldEqual, "Victory")
}

func TestFire_failureDoesNotStampCooldown(t *testing.T) {
	r := newTestReactor(t, validConfig(), func(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
		return nil, errors.New("another draw is already running")
	})
	r.fire(context.Background(), "Victory")
	test.That(t, r.firedCount, test.ShouldEqual, 0)
	test.That(t, r.lastFiredAt.IsZero(), test.ShouldBeTrue)
	test.That(t, r.cooldownElapsed(), test.ShouldBeTrue)
}

func TestCooldown_blocksUntilElapsed(t *testing.T) {
	cfg := validConfig()
	cfg.CooldownSec = 3600
	r := newTestReactor(t, cfg, func(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
		return nil, nil
	})
	test.That(t, r.cooldownElapsed(), test.ShouldBeTrue)
	r.fire(context.Background(), "Victory")
	test.That(t, r.cooldownElapsed(), test.ShouldBeFalse)
}

func TestTrigger_unknownLabel(t *testing.T) {
	r := newTestReactor(t, validConfig(), nil)
	_, err := r.triggerLabel(context.Background(), "Nope")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "label_commands")
}

func TestTrigger_missingLabel(t *testing.T) {
	r := newTestReactor(t, validConfig(), nil)
	_, err := r.triggerLabel(context.Background(), "")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "label")
}

func TestTrigger_sendsCommand(t *testing.T) {
	called := false
	r := newTestReactor(t, validConfig(), func(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
		called = true
		return map[string]interface{}{"total_points": 658}, nil
	})
	resp, err := r.triggerLabel(context.Background(), "Victory")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, called, test.ShouldBeTrue)
	test.That(t, resp["triggered"], test.ShouldEqual, "Victory")
}

func TestDoCommand_unknownCommand(t *testing.T) {
	r := newTestReactor(t, validConfig(), nil)
	_, err := r.DoCommand(context.Background(), map[string]interface{}{"command": "nope"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "unknown command")
}

func TestStatus_reportsIdleBeforeStart(t *testing.T) {
	r := newTestReactor(t, validConfig(), nil)
	status := r.status()
	test.That(t, status["reacting"], test.ShouldEqual, false)
	test.That(t, status["target"], test.ShouldEqual, "drawer")
	test.That(t, status["fired_count"], test.ShouldEqual, 0)
}
