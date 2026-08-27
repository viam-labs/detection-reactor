# detection-reactor

`viam:detection-reactor:reactor` is a Viam `generic` service that sends a **DoCommand when a vision service reports a labelled detection**.

It knows nothing about either end. You give it a label and the exact DoCommand payload to send, and it passes that payload through untouched — so anything a vision service can detect can drive anything that implements DoCommand.

```
camera ──> vision service ──> detection-reactor ──> any resource's DoCommand
```

## Setup

1. **Add the module.** In the [Viam app](https://app.viam.com), go to your machine's **CONFIGURE** tab, click **+**, and search the registry for `detection-reactor`. Add the `reactor` generic service.

2. **Point it at a vision service, a camera, and a target** — see [Configuration](#configuration). All three are declared as implicit dependencies, so they do not need to be listed in `depends_on`.

3. **Save**, then start it with `{"command": "start_reacting"}`. The service starts idle on purpose; see [Behavior and caveats](#behavior-and-caveats).

## Configuration

```json
{
  "vision_service": "gestures",
  "camera": "camera-1",
  "target": "drawer",
  "label_commands": {
    "Victory": { "capture_and_draw": {} }
  }
}
```

`vision_service`, `camera`, `target`, and `label_commands` are required; everything else has a default.

A bare `target` is read as a generic service. Prefix it with an API to reach anything else — `"rdk:component:sensor/arm-recorder"`.

See the [reactor documentation](./docs/viam_detection-reactor_reactor.md) for every optional attribute and the full `DoCommand` reference.

## Behavior and caveats

- **It starts idle.** A config change, a module update, or a machine restart never sets a target running on its own — you send `start_reacting` deliberately. For a target that moves hardware, an orchestrator that auto-arms on reconfigure is a hazard, not a convenience.

- **Commands are sent synchronously.** The poll loop waits for the target to finish, so an action taking minutes cannot overlap itself and no detection is acted on while it runs. Cooldown is measured from completion, not from the start. `stop_reacting` cancels the in-flight command along with the loop, so it halts a long action rather than waiting it out.

- **A failed command does not stamp the cooldown.** If the target rejects a command — busy, misconfigured, unreachable — the next poll may try again rather than going quiet for a cooldown period. A rejection is not a reaction.

- **The payload is static.** The target receives exactly what is in `label_commands`. The detection itself — bounding box, score, which label matched — is not passed through, so this cannot express "act on where the thing was seen."

- **Debouncing belongs upstream.** This service reacts to whatever the vision service reports each poll. If a held pose should fire once rather than continuously, the vision service must emit on the rising edge; `cooldown_sec` is a backstop, not a substitute.

- **One reaction per poll.** When several mapped labels appear in one frame, the highest-confidence one wins and the rest are ignored.

- **This is not a safety mechanism.** It depends on the camera, the vision service, this service, and the network all being healthy. Anything that moves physical hardware needs an out-of-band stop that does not route through here.

## Manual validation

These steps verify the module on a real machine. Use the **Control** tab in the [Viam app](https://app.viam.com) or the `viam machine part run` CLI.

**Prerequisites:** the service is configured and the machine is online; the vision service, camera, and target all build cleanly.

1. **Confirm the service loaded.**
   ```json
   {"command": "status"}
   ```
   Verify `reacting` is `false`, `target` matches your config, and `labels` lists exactly the keys of `label_commands`.

2. **Prove the target wiring, without the camera.**
   ```json
   {"command": "trigger", "label": "Victory"}
   ```
   The target should act, and the response should carry its reply under `response`. If this fails, the problem is the target or the payload — not detection.

3. **Start reacting.**
   ```json
   {"command": "start_reacting"}
   ```
   Send `status` twice a few seconds apart and confirm `polls` climbs. If it does not, the vision service is erroring — check `last_poll_error`.

4. **Trigger it for real.** Present whatever the vision service detects. Confirm the target acts, then check `status` for `last_label` and a small `seconds_since_last_fired`.

5. **Confirm the cooldown.** Trigger again immediately. Within `cooldown_sec` nothing should happen and `fired_count` should not move.

6. **Confirm it stops.**
   ```json
   {"command": "stop_reacting"}
   ```
   `status` should report `reacting: false` and `polls` should stop climbing.

### Using the Viam CLI

```bash
viam machine part run --part <part-id> --service detection-reactor --method DoCommand --data '{"command":{"command":"status"}}'
```

Note the payload nests twice — the CLI's `command` wrapper, then the service's own `command` key.
