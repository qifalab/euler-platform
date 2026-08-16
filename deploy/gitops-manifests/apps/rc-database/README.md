# rc-database — planned-component placeholder chart

This chart currently has **no corresponding code under `services/`**. It is
kept deliberately: the architecture docs list rc-database as a planned component
(see `docs/architecture/11-adjudication-decisions.md` and
`docs/architecture/09-roadmap.md`). The chart deploys nothing useful until the
service image exists; the `harbor.internal/cloudplatform/rc-database` repository
is a placeholder and Argo CD will show the Application degraded until the
image is published. Remove this README when the service lands.
