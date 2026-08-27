# Model `viam:detection-reactor:reactor`

A Viam `generic` service that polls a vision service and sends a configured DoCommand when a mapped label is detected.

## Configuration

```json
{
  "vision_service": "gestures",
  "camera": "camera-1",
  "target": "drawer",
  "label_commands": {
    "Victory": { "capture_and_draw": {} }
  },
  "poll_interval_ms": 500,
  "min_confidence": 0.5,
  "cooldown_sec": 5
}
```

### Attributes

| Name | Type | Inclusion | Description |
|---|---|---|---|
| `vision_service` | string | **Required** | The vision service to poll. Must implement `GetDetections`. Declared as an implicit dependency. |
| `camera` | string | **Required** | Camera name passed to `DetectionsFromCamera`. Declared as an implicit dependency. Vision services that are pinned to one camera will reject a mismatch. |
| `target` | string | **Required** | Resource receiving the commands. A bare name resolves as `rdk:service:generic`; prefix with an API to reach anything else, e.g. `rdk:component:sensor/arm-recorder`. Declared as an implicit dependency. |
| `label_commands` | object | **Required** | Maps a detection label to the DoCommand payload sent to `target`. Payloads are passed through untouched. Every entry must be non-empty. |
| `poll_interval_ms` | number | Optional | How often to poll the vision service. Default: `500` |
| `min_confidence` | number | Optional | Detections scoring below this are ignored, `[0, 1]`. Default: `0.5` |
| `cooldown_sec` | number | Optional | Minimum gap between reactions, measured from the completion of the previous one. Default: `5` |

## Behavior

Each poll fetches detections, discards those below `min_confidence` or whose label is not in `label_commands`, and takes the highest-confidence survivor. If the cooldown has elapsed, its payload is sent to `target` and the loop blocks until the target returns.

A successful command stamps the cooldown. A failed one does not, and is logged.

The loop only runs between `start_reacting` and `stop_reacting`. Closing or rebuilding the resource stops it.

## DoCommand reference

### `start_reacting`

Starts the poll loop. Idempotent — starting an already-running reactor returns `already: true` and changes nothing.

```json
{"command": "start_reacting"}
```
```json
{"reacting": true}
```

### `stop_reacting`

Stops the poll loop and cancels any command currently in flight, then waits for the loop to exit before returning.

```json
{"command": "stop_reacting"}
```
```json
{"reacting": false}
```

### `trigger`

Sends one label's payload immediately, bypassing the vision service, and returns the target's response. Works whether or not the loop is running, and stamps the cooldown like a real reaction.

```json
{"command": "trigger", "label": "Victory"}
```
```json
{"triggered": "Victory", "response": {"total_points": 658}}
```

### `status`

```json
{"command": "status"}
```
```json
{
  "reacting": true,
  "reacting_since": "2026-08-26T20:14:02Z",
  "target": "drawer",
  "labels": ["Victory"],
  "polls": 412,
  "fired_count": 3,
  "last_label": "Victory",
  "last_fired_at": "2026-08-26T20:31:18Z",
  "seconds_since_last_fired": 96.4
}
```

`last_poll_error` appears only when the most recent poll failed.

## Safety

The filters here reduce spurious triggering; they do not make triggering safe. This service depends on the camera, the vision service, and the network all being healthy, and if it fails, whatever protection it provided fails with it. Anything moving physical hardware in response to a detection needs an out-of-band stop that does not route through this service.
