# Administrator CLI and Tenant Operations

## Overview

Buddy Sandboxes supports multi-tenant isolation where each user is assigned a dedicated Kubernetes namespace and accesses the API via scoped bearer tokens. While the gateway exposes administrative REST endpoints under `/api/v1/admin/`, the developer CLI (`buddy-sbx`) previously lacked administrative commands, requiring cluster operators to invoke raw HTTP requests or in-cluster database commands.

This specification describes the **`buddy-sbx admin`** command hierarchy, enabling authorized administrators to manage platform users, issue and revoke access tokens, and inspect sandboxes across tenant namespaces directly from their workstations over secure HTTPS.

---

## 1. Command Hierarchy & UX

The administrative interface is organized under the `buddy-sbx admin` command group:

```text
buddy-sbx admin
├── users
│   ├── ls                          # List all registered users
│   ├── create <display-name>       # Create a new user with an assigned namespace
│   ├── disable <user-id>           # Deactivate a user account
│   ├── enable <user-id>            # Reactivate a user account
│   └── set-role <user-id> <role>   # Update user role (admin | user)
├── tokens
│   ├── issue <user-id>             # Issue an access token for a user
│   └── revoke <user-id> <token-id> # Revoke an issued access token
└── sandboxes
    ├── ls <user-id>                # List sandboxes in a user's namespace
    └── rm <user-id> <sandbox-name> # Terminate a user's sandbox
```

### User Management Workflows
* **Listing Users**: `buddy-sbx admin users ls` renders a formatted table showing User ID, Display Name, Tenant Namespace, Role, Status (`active` or `disabled`), and Creation Timestamp.
* **Provisioning Users**: `buddy-sbx admin users create "Alice Smith" --namespace team-alice` creates the user identity bound to the specified namespace.
* **Account Status**: `buddy-sbx admin users disable <user-id>` prevents subsequent token authentication; `enable` restores access.
* **Role Management**: `buddy-sbx admin users set-role <user-id> admin` promotes a user to administrator.

### Token Management Workflows
* **Token Issuance**: `buddy-sbx admin tokens issue <user-id>` generates an opaque, high-entropy bearer token (`bb_sec_...`). The token value is printed once to `stdout` alongside its unique Token ID and expiration date.
* **Token Revocation**: `buddy-sbx admin tokens revoke <user-id> <token-id>` invalidates the token across all API replicas immediately.

### Cross-Tenant Sandbox Operations
* **Inspecting Workloads**: `buddy-sbx admin sandboxes ls <user-id>` queries `/api/v1/admin/users/{userID}/sandboxes` and displays all running or completed sandboxes belonging to the target tenant.
* **Administrative Teardown**: `buddy-sbx admin sandboxes rm <user-id> <sandbox-name>` terminates runaway or stuck sandboxes within a user's tenant boundary.

---

## 2. Security Boundaries & Authorization

1. **Gateway-Enforced RBAC**:
   * Every `/api/v1/admin/*` endpoint validates that the caller's principal has `Role == "admin"`.
   * Non-admin callers receive HTTP `403 Forbidden`.
2. **Ephemeral Display of Secrets**:
   * Plaintext token values are only returned at creation time over TLS (`https://`).
   * Only SHA-256 hashes of tokens are stored in the PostgreSQL database.
3. **Audit Trail**:
   * All administrative mutations (`create_user`, `set_user_disabled`, `set_user_role`, `issue_user_token`, `revoke_user_token`) generate immutable audit records in the database with the acting administrator's ID and target entity ID.
