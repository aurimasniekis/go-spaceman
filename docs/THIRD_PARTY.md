# Third-party components and attribution

Release builds generate the full dependency list as SPDX SBOMs (`make sbom`). This file records components whose licence or
attribution needs specific mention.

| Component | Use | Licence | Notes |
|---|---|---|---|
| herdr-core (`hc`) | Runner, invoked as an external binary | Apache-2.0 | Not vendored. Version recorded in node capability inventory. |
| go-pulse | Event bus client | Apache-2.0 | Go module dependency. |
| go-pulse-github | Event contract reference for seed templates | MIT | Contract facts only; fixtures used in tests are cited by path and revision. |
| Night Owl colour theme (Sarah Drasner) | UI and site palette inspiration | MIT | Palette values adapted; attribution kept here. |
