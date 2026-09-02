// Package reactor implements a Viam generic service that sends a DoCommand
// when a vision service reports a labelled detection.
package reactor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/vision"
	"go.viam.com/rdk/vision/objectdetection"
)

// Model is the detection-reactor service.
var Model = resource.NewModel("viam", "detection-reactor", "reactor")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newReactor,
		},
	)
}

// Config is the detection-reactor service configuration.
type Config struct {
	VisionService string `json:"vision_service"`
	Camera        string `json:"camera"`
	// Target is the resource the mapped commands are sent to. A bare name is
	// read as a generic service; prefix it with an API ("rdk:component:sensor/
	// arm-recorder") to reach anything else.
	Target string `json:"target"`
	// LabelCommands maps a detection label to the DoCommand payload sent to
	// Target. The payload is passed through untouched, so this service needs
	// to know nothing about the target's command vocabulary.
	LabelCommands  map[string]map[string]interface{} `json:"label_commands"`
	PollIntervalMs int                               `json:"poll_interval_ms,omitempty"`
	MinConfidence  float64                           `json:"min_confidence,omitempty"`
	CooldownSec    float64                           `json:"cooldown_sec,omitempty"`
	// CommandTimeoutSec bounds how long a target may take to answer a
	// DoCommand. Leave it at 0 to inherit the RDK's DefaultMethodTimeout,
	// which is 10 minutes: past that the call is cancelled and the
	// cancellation propagates into the target, aborting whatever it was
	// doing. Raise this when the target's work legitimately runs longer.
	CommandTimeoutSec float64 `json:"command_timeout_sec,omitempty"`
}

const (
	defaultPollIntervalMs = 500
	defaultMinConfidence  = 0.5
	defaultCooldownSec    = 5.0
	defaultTargetAPI      = "rdk:service:generic"
)

// TargetName resolves the configured target to a fully qualified resource name.
func (cfg *Config) TargetName() (resource.Name, error) {
	target := cfg.Target
	if !strings.Contains(target, "/") {
		target = defaultTargetAPI + "/" + target
	}
	name, err := resource.NewFromString(target)
	if err != nil {
		return resource.Name{}, fmt.Errorf("target %q is not a resource name: %w", cfg.Target, err)
	}
	return name, nil
}

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.VisionService == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "vision_service")
	}
	if cfg.Camera == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "camera")
	}
	if cfg.Target == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "target")
	}
	if len(cfg.LabelCommands) == 0 {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "label_commands")
	}
	for label, cmd := range cfg.LabelCommands {
		if len(cmd) == 0 {
			return nil, nil, fmt.Errorf("label_commands[%q] is empty; give it the DoCommand payload to send", label)
		}
	}
	if cfg.PollIntervalMs < 0 {
		return nil, nil, fmt.Errorf("poll_interval_ms must be >= 0 (0 uses the default), got %d", cfg.PollIntervalMs)
	}
	if cfg.MinConfidence < 0 || cfg.MinConfidence > 1 {
		return nil, nil, fmt.Errorf("min_confidence must be in [0, 1], got %g", cfg.MinConfidence)
	}
	if cfg.CooldownSec < 0 {
		return nil, nil, fmt.Errorf("cooldown_sec must be >= 0, got %g", cfg.CooldownSec)
	}
	if cfg.CommandTimeoutSec < 0 {
		return nil, nil, fmt.Errorf(
			"command_timeout_sec must be >= 0 (0 inherits the RDK default), got %g", cfg.CommandTimeoutSec,
		)
	}
	target, err := cfg.TargetName()
	if err != nil {
		return nil, nil, err
	}
	return []string{cfg.VisionService, cfg.Camera, target.String()}, nil, nil
}

type reactor struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	cfg    *Config
	vision vision.Service
	target resource.Resource

	mu            sync.Mutex
	cancelFn      context.CancelFunc
	done          chan struct{}
	lastLabel     string
	lastFiredAt   time.Time
	firedCount    int
	pollCount     int
	lastPollErr   string
	reactingSince time.Time
}

func newReactor(
	_ context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	if cfg.PollIntervalMs == 0 {
		cfg.PollIntervalMs = defaultPollIntervalMs
	}
	if cfg.MinConfidence == 0 {
		cfg.MinConfidence = defaultMinConfidence
	}
	if cfg.CooldownSec == 0 {
		cfg.CooldownSec = defaultCooldownSec
	}
	visionSvc, err := vision.FromProvider(deps, cfg.VisionService)
	if err != nil {
		return nil, fmt.Errorf("reactor: get vision_service dep %q: %w", cfg.VisionService, err)
	}
	targetName, err := cfg.TargetName()
	if err != nil {
		return nil, fmt.Errorf("reactor: %w", err)
	}
	target, ok := deps[targetName]
	if !ok {
		return nil, fmt.Errorf("reactor: target %q is not among this service's dependencies", targetName)
	}
	return &reactor{
		name:   conf.ResourceName(),
		logger: logger,
		cfg:    cfg,
		vision: visionSvc,
		target: target,
	}, nil
}

func (r *reactor) Name() resource.Name {
	return r.name
}

// Close stops the poll loop so a rebuild cannot leave one running.
func (r *reactor) Close(_ context.Context) error {
	_, _ = r.stopReacting()
	return nil
}

func (r *reactor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "start_reacting":
		return r.startReacting()
	case "stop_reacting":
		return r.stopReacting()
	case "trigger":
		label, _ := cmd["label"].(string)
		return r.triggerLabel(ctx, label)
	case "status":
		return r.status(), nil
	default:
		return nil, fmt.Errorf(
			"reactor: unknown command %q; expected \"start_reacting\", \"stop_reacting\", \"trigger\", or \"status\"", command,
		)
	}
}

func (r *reactor) startReacting() (map[string]interface{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelFn != nil {
		return map[string]interface{}{"reacting": true, "already": true}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cancelFn = cancel
	r.done = done
	r.reactingSince = time.Now()
	go r.reactLoop(ctx, done)
	r.logger.Infof("reactor: reacting to %d labels from %q every %dms",
		len(r.cfg.LabelCommands), r.cfg.VisionService, r.cfg.PollIntervalMs)
	return map[string]interface{}{"reacting": true}, nil
}

func (r *reactor) stopReacting() (map[string]interface{}, error) {
	r.mu.Lock()
	cancel, done := r.cancelFn, r.done
	r.cancelFn, r.done = nil, nil
	r.mu.Unlock()
	if cancel == nil {
		return map[string]interface{}{"reacting": false, "already": true}, nil
	}
	cancel()
	<-done
	r.logger.Info("reactor: stopped reacting")
	return map[string]interface{}{"reacting": false}, nil
}

func (r *reactor) reactLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Duration(r.cfg.PollIntervalMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		detections, err := r.vision.DetectionsFromCamera(ctx, r.cfg.Camera, nil)
		r.mu.Lock()
		r.pollCount++
		if err != nil {
			r.lastPollErr = err.Error()
		} else {
			r.lastPollErr = ""
		}
		r.mu.Unlock()
		if err != nil {
			if ctx.Err() == nil {
				r.logger.Debugf("reactor: poll %q: %v", r.cfg.VisionService, err)
			}
			continue
		}
		label := r.pickLabel(detections)
		if label == "" || !r.cooldownElapsed() {
			continue
		}
		// Fired synchronously: a target whose command takes minutes holds the
		// loop, so a long action can never overlap itself, and stop_reacting
		// cancels the action along with the loop.
		r.fire(ctx, label)
	}
}

// pickLabel returns the highest-confidence mapped label above min_confidence.
func (r *reactor) pickLabel(detections []objectdetection.Detection) string {
	best, bestScore := "", 0.0
	for _, d := range detections {
		if d.Score() < r.cfg.MinConfidence {
			continue
		}
		if _, mapped := r.cfg.LabelCommands[d.Label()]; !mapped {
			continue
		}
		if d.Score() > bestScore {
			best, bestScore = d.Label(), d.Score()
		}
	}
	return best
}

func (r *reactor) cooldownElapsed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastFiredAt.IsZero() || time.Since(r.lastFiredAt) >= time.Duration(r.cfg.CooldownSec*float64(time.Second))
}

// commandCtx applies command_timeout_sec to ctx. With the attribute unset the
// context is returned untouched, which leaves the RDK's own interceptor to
// stamp DefaultMethodTimeout (10 minutes) on the outbound call. Note that this
// can only ever shorten an inbound request context: a deadline already carried
// by ctx wins, because a child context cannot outlive its parent.
func (r *reactor) commandCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.cfg.CommandTimeoutSec <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(r.cfg.CommandTimeoutSec*float64(time.Second)))
}

func (r *reactor) fire(ctx context.Context, label string) {
	cmd := r.cfg.LabelCommands[label]
	r.logger.Infof("reactor: %q -> sending %v to %q", label, keysOf(cmd), r.cfg.Target)
	ctx, cancel := r.commandCtx(ctx)
	defer cancel()
	if _, err := r.target.DoCommand(ctx, cmd); err != nil {
		// No cooldown stamp: a rejected command should be retried, not
		// silently swallowed until the cooldown lapses.
		if ctx.Err() == nil {
			r.logger.Infof("reactor: %q -> %q failed: %v", label, r.cfg.Target, err)
		}
		return
	}
	r.mu.Lock()
	r.lastLabel = label
	r.lastFiredAt = time.Now()
	r.firedCount++
	r.mu.Unlock()
	r.logger.Infof("reactor: reacted to %q", label)
}

func (r *reactor) triggerLabel(ctx context.Context, label string) (map[string]interface{}, error) {
	if label == "" {
		return nil, errors.New("reactor: trigger requires a \"label\"")
	}
	cmd, ok := r.cfg.LabelCommands[label]
	if !ok {
		return nil, fmt.Errorf("reactor: label %q is not in label_commands", label)
	}
	// Bounded by the inbound request's own deadline, so this cannot carry a
	// trigger past the caller's timeout; the polling path is the one that
	// benefits from a raised command_timeout_sec.
	ctx, cancel := r.commandCtx(ctx)
	defer cancel()
	resp, err := r.target.DoCommand(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("reactor: trigger %q -> %q: %w", label, r.cfg.Target, err)
	}
	r.mu.Lock()
	r.lastLabel = label
	r.lastFiredAt = time.Now()
	r.firedCount++
	r.mu.Unlock()
	return map[string]interface{}{"triggered": label, "response": resp}, nil
}

func keysOf(cmd map[string]interface{}) []string {
	out := make([]string, 0, len(cmd))
	for k := range cmd {
		out = append(out, k)
	}
	return out
}

func (r *reactor) status() map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]interface{}{
		"reacting":    r.cancelFn != nil,
		"target":      r.cfg.Target,
		"labels":      keysOf(mapToAny(r.cfg.LabelCommands)),
		"polls":       r.pollCount,
		"fired_count": r.firedCount,
	}
	if r.cfg.CommandTimeoutSec > 0 {
		out["command_timeout_sec"] = r.cfg.CommandTimeoutSec
	}
	if !r.reactingSince.IsZero() && r.cancelFn != nil {
		out["reacting_since"] = r.reactingSince.UTC().Format(time.RFC3339)
	}
	if r.lastLabel != "" {
		out["last_label"] = r.lastLabel
		out["last_fired_at"] = r.lastFiredAt.UTC().Format(time.RFC3339)
		out["seconds_since_last_fired"] = time.Since(r.lastFiredAt).Seconds()
	}
	if r.lastPollErr != "" {
		out["last_poll_error"] = r.lastPollErr
	}
	return out
}

func mapToAny(in map[string]map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *reactor) Status(_ context.Context) (map[string]interface{}, error) {
	return r.status(), nil
}
