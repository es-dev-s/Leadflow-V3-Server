# LeadFlow Backend (Go)

PostgreSQL-backed HTTP API with JWT auth and RBAC.

## Setup

`.env`:

```bash
DATABASE_URL=postgresql://user:pass@host:5432/dbname
JWT_SECRET=change-me-to-a-long-random-secret

# Change this anytime — restart the binary after editing.
PORT=9080
HOST=0.0.0.0

# Production hardening
APP_ENV=production
COOKIE_SECURE=true
COOKIE_SAMESITE=Lax
# Comma-separated browser origins allowed for credentialed CORS.
# Prefer same-origin reverse proxy to the UI; set this if the browser calls the API host directly.
CORS_ORIGINS=https://app.example.com
```

Schema changes ship as versioned SQL under `migrations/`. Startup applies them once via `schema_migrations` — it no longer runs ad-hoc DDL.

```bash
cd backend
go run .
# listens on HOST:PORT from .env (default 0.0.0.0:9080)
```

UI (`lead_flow_ui/.env.local`) should point `BACKEND_URL` at the same CRM port, and set `PORT=3100` (or any free port) for Next.
## Roles (RBAC)

| Role value | Label |
|---|---|
| `SUPERADMIN` | Superadmin |
| `ANALYST_TEAM_LEAD` | Analyst Team Lead |
| `LEAD_ANALYST` | Lead Analyst |
| `MAIN_TEAM_LEAD` | Main Team Lead |
| `SALES_EXECUTIVE` | Sales Executive |

- Superadmin, Analyst Team Lead, and Main Team Lead can create users in their role scope.
- Scope-based permissions apply to leads (creator / team / assignee).

## Auth

| Method | Path | Access | Description |
|---|---|---|---|
| POST | `/api/auth/login` | Public | Email + password → JWT |
| GET | `/api/auth/me` | Auth | Current user (DB-backed) |
| GET | `/api/roles` | Auth | Available RBAC roles |
| GET | `/api/users` | Auth | List users |
| POST | `/api/users` | Superadmin / ATL / MTL | Create user (scoped) |
| PATCH | `/api/users/{id}` | Superadmin / ATL / MTL | Update role / password (scoped) |
| * | `/api/leads*` | Auth | Leads + reporting |
| GET | `/api/transfers` | Auth | Transfer logs |
| GET | `/health` | Public | Service + DB health |

Pass `Authorization: Bearer <token>` on protected routes.

### Security notes

- Passwords hashed with bcrypt (legacy plaintext upgraded on successful login)
- JWT HS256, 24h TTL, issuer `leadflow-backend`
- Browsers authenticate with an **HttpOnly** cookie (`leadflow_access`); JS never stores the JWT
- API/automation clients may still send `Authorization: Bearer <token>`
- SSE uses cookie/Bearer only — query-string tokens are rejected
- CORS is an explicit `CORS_ORIGINS` allowlist (includes `PUT`); origins are never reflected
- Every authenticated CRM request reloads the user from DB (role/deletion enforced live)
- Login rate-limited (12 attempts / 10 minutes per IP+email)
- New passwords require ≥8 chars with at least one letter and one number
- `POST /api/auth/logout` clears the session cookie

### Bulk lead seed (resilience testing)

Separate from the API process. Seeds synthetic leads tagged `[seed-bulk]`, randomly owned by existing Lead Analysts and assigned to Team Leads / Sales Executives:

```bash
cd backend
go run ./cmd/seedleads -target 1000000          # ensure ≥1M leads
go run ./cmd/seedleads -count 50000             # insert exactly N
go run ./cmd/seedleads -target 1000000 -fast=false
```

`-fast` (default for large loads) temporarily drops secondary `Lead` indexes, COPYs rows, then rebuilds indexes.
