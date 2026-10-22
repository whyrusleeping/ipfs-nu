// Package netcheck measures a compact NAT profile (docs/networking.md D11c
// step 1): mapping, port allocation, filtering, mapping lifetime, hairpin,
// IPv6 and UDP availability, using /nu/netcheck/1 observers (served by
// `nu relay`). Profiles are address-free and exchanged on /nu/natprofile/1.
//
// Results are measurements against the observers actually used, not promises
// about arbitrary destinations; consumers must treat them as hints for probe
// scheduling and escalation, never as authorization.
package netcheck
