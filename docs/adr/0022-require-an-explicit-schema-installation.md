---
status: accepted
---

# Require an explicit schema installation

Initialize the complete FRY schema and a version ledger in one transaction, and
require compatible recorded SQL and catalog structure before production commands
open mail storage. Reject unversioned prototype databases instead of inferring
their history or silently adopting them: separate component commits could leave
partial installations, and identical table names do not establish compatibility.

The first contract supports version 1, the same schema name and PostgreSQL major,
with explicit initialization and a read-only check. It supplies no upgrade or
legacy conversion; future changes need registered, reviewed transitions and
their own data-preservation evidence. See the [installation design](../design/schema-installation.md).
