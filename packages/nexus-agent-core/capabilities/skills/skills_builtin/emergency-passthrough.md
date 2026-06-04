---
name: emergency-passthrough
description: Walk an emergency — assess the kill switch + passthrough state and propose a safe response
allowed-tools: observe_killswitch, observe_passthrough, observe_health, observe_alerts, navigate
---

# Emergency passthrough / kill-switch drill

Use this in an emergency where the operator may need to bypass compliance hooks or
halt the fleet (a hook storm, a bad rule blocking all traffic, an upstream incident).

1. Read the current safety state FIRST: `observe_killswitch` (engaged or not, by whom,
   when) and `observe_passthrough` (global + per-adapter + per-provider overrides, and
   what each bypasses). State exactly what is and isn't currently bypassed.
2. Establish the trigger: `observe_health` + `observe_alerts` — is traffic actually
   being blocked/failing, or is this precautionary? Cite the numbers.
3. Pick the SMALLEST sufficient lever: a per-provider passthrough beats a global one;
   global passthrough beats the kill switch. Explain the trade-off (passthrough lets
   traffic through while skipping hooks/cache/normalize; the kill switch halts TLS
   bumping fleet-wide).
4. PROPOSE the specific action with a reason and an expiry in mind — engaging
   passthrough or the kill switch is a high-gravity write the operator MUST authorize
   on the gate (prod shows the red banner). Never engage either without confirmation,
   and recommend the narrowest tier that resolves the incident.
