# acl-users (kuttl TestCase — SKELETON, owner: OPS-8.1)

ACL user provisioning + multi-password rotation (arch §7 security). Floor: ACL works on
7.x+. Suggested flow:

1. `00`–`02` cert-manager + operator + cluster with `spec.users[]` (allow/deny command
   categories, key patterns, `passwordSecret` with multiple keys for rotation).
2. `03-verify-acl` — `exec_valkey ... ACL GETUSER app` shows the rendered rule; an
   allowed command succeeds and a denied one (e.g. `FLUSHALL`) is rejected.
3. `04-rotate` — flip the active password key; assert the old password still works
   during the rotation window, then is retired.

Golden: optionally `compare/` the rendered ACL Secret (`internal-<cluster>-acl`).
Keep the engine `master`/`slave` tokens verbatim where INFO is grepped (arch §2.3 note).
