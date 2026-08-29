# Unsupported

Fields and behaviours cloudrig accepts but does not honour, and operations it
declines outright. Kept current per rule 5 in [spec.md](spec.md): unimplemented
must be loud, and nothing may be accepted-and-ignored silently without appearing
here.

## Returns 501 / `codes.Unimplemented`

Nothing yet — no service handlers exist as of milestone 1 step 0.

Planned for M1, naming the operation in the error: resumable uploads, the XML
API, compose, rewrite, and object versioning reads. See [PLAN.md](PLAN.md) for
why resumable in particular stays out (D1).

## Accepted but ignored

Nothing yet.

## Known divergence from real GCS

- **Orphaned generations.** A writer that loses an `ifGenerationMatch` CAS has
  already written its object metadata and blob. In M1 those are left as garbage
  and reclaimed only by `Reset`. Real GCS leaves nothing behind. See PLAN.md.
