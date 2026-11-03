# Project status

SpaceMan was built in five milestones of working vertical slices: the foundation and first runs (M1), coordination
(M2), configuration, knowledge and memory (M3), hardening (M4) and completion (M5). The features of M1 to M4 are
implemented; M5 added the Terraform provider, the command line, the scripted end-to-end demonstration and this site. No
version has been released, and the release-readiness review (M5-05) has not been done. A `partial` row names what is
missing in its notes, and `n/a` marks a conditional requirement that does not apply (a feature that was not added).

Each requirement below is listed with its state and evidence. **Simulated** evidence comes from automated tests with
fakes or embedded dependencies; **real** evidence comes from tests against the real dependency (named, with the date).
A requirement without real evidence has not been verified against the real system yet.

<!--@include: ../../docs/TRACEABILITY.md-->
