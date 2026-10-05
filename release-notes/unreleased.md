# Unreleased

- Command sandbox cleanup now reclaims nested read-only scratch directories
  before removal, using anchored no-follow directory handles. File modes and
  outside symlink targets are untouched; cleanup failures still report and
  stale entries are retried independently. Capability claims are unchanged.
  Fixes the 2026-10-05 incident in which 271 read-only checkout directories
  survived every sweep and held the crew stalled across restarts for ~7 hours.
  macOS also reclaims nonempty 0000 directories using an anchored,
  directory-only NOFOLLOW_ANY bootstrap before handle verification. Unsupported
  kernels retain the existing cleanup failure instead of following links.
