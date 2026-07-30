#!/usr/bin/env python3
"""Concurrent auth + RBAC verification against a running LeadFlow API."""

from __future__ import annotations

import concurrent.futures
import json
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"
SUPER_EMAIL = "superadmin@demo.local"
SUPER_PASSWORD = "LeadFlow1!"
PASSWORD = "LeadFlow1!"


@dataclass
class Account:
    email: str
    role: str
    name: str
    password: str = PASSWORD
    token: str = ""
    user_id: str = ""


ACCOUNTS = [
    Account("rbac.se@demo.local", "SALES_EXECUTIVE", "RBAC SE"),
    Account("rbac.mtl@demo.local", "MAIN_TEAM_LEAD", "RBAC MTL"),
    Account("rbac.atl@demo.local", "ANALYST_TEAM_LEAD", "RBAC ATL"),
    Account("rbac.la@demo.local", "LEAD_ANALYST", "RBAC LA"),
    Account("rbac.support@demo.local", "SUPPORT", "RBAC Support"),
]


def req(method: str, path: str, body: dict | None = None, token: str | None = None):
    data = None if body is None else json.dumps(body).encode()
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(
        f"{BASE}{path}", data=data, headers=headers, method=method
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as res:
            raw = res.read().decode()
            return res.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            payload = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            payload = {"error": raw}
        return e.code, payload


def login(email: str, password: str):
    return req("POST", "/api/auth/login", {"email": email, "password": password})


def ensure_accounts(super_token: str) -> None:
    for acct in ACCOUNTS:
        status, body = req(
            "POST",
            "/api/users",
            {
                "name": acct.name,
                "email": acct.email,
                "password": acct.password,
                "role": acct.role,
            },
            token=super_token,
        )
        if status in (200, 201):
            acct.user_id = body["user"]["id"]
            print(f"  created {acct.email} ({acct.role})")
        elif status == 409:
            print(f"  exists  {acct.email}")
        else:
            raise SystemExit(f"create {acct.email} failed: {status} {body}")


def concurrent_login(rounds: int = 8) -> list[tuple[Account, dict]]:
    jobs: list[Account] = []
    for _ in range(rounds):
        jobs.extend(ACCOUNTS)
        jobs.append(
            Account(SUPER_EMAIL, "SUPERADMIN", "Super Admin", SUPER_PASSWORD)
        )

    results: list[tuple[Account, dict]] = []
    errors: list[str] = []

    def one(acct: Account):
        status, body = login(acct.email, acct.password)
        if status != 200:
            return acct, {"_error": f"login {status}: {body}"}
        return acct, body

    with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
        for acct, body in pool.map(one, jobs):
            if "_error" in body:
                errors.append(f"{acct.email}: {body['_error']}")
            else:
                results.append((acct, body))

    if errors:
        raise SystemExit("concurrent login failures:\n  " + "\n  ".join(errors))
    return results


def verify_identity_no_mix(results: list[tuple[Account, dict]]) -> None:
    tokens = set()
    for acct, body in results:
        token = body["token"]
        user = body["user"]
        tokens.add(token)
        if user["email"].lower() != acct.email.lower():
            raise SystemExit(
                f"identity mix on login: expected {acct.email}, got {user['email']}"
            )
        if user["role"] != acct.role and acct.email != SUPER_EMAIL:
            raise SystemExit(
                f"role mix on login: expected {acct.role}, got {user['role']}"
            )
        # Persist latest token per account email
        for target in ACCOUNTS:
            if target.email == acct.email:
                target.token = token
                target.user_id = user["id"]
        if acct.email == SUPER_EMAIL:
            acct.token = token
            acct.user_id = user["id"]

    if len(tokens) < len(ACCOUNTS):
        raise SystemExit("expected distinct tokens across roles")


def concurrent_me_checks() -> None:
    sessions = list(ACCOUNTS) + [
        Account(SUPER_EMAIL, "SUPERADMIN", "Super Admin", SUPER_PASSWORD, token="")
    ]
    # refresh super token
    status, body = login(SUPER_EMAIL, SUPER_PASSWORD)
    if status != 200:
        raise SystemExit(f"super re-login failed: {status} {body}")
    sessions[-1].token = body["token"]
    sessions[-1].user_id = body["user"]["id"]
    sessions[-1].role = body["user"]["role"]

    errors: list[str] = []

    def check(acct: Account):
        status, body = req("GET", "/api/auth/me", token=acct.token)
        if status != 200:
            return f"{acct.email}: /me {status} {body}"
        user = body["user"]
        if user["email"].lower() != acct.email.lower():
            return f"ME MIX: token for {acct.email} returned {user['email']}"
        if user["role"] != acct.role:
            return f"ROLE MIX: {acct.email} expected {acct.role} got {user['role']}"
        return None

    with concurrent.futures.ThreadPoolExecutor(max_workers=24) as pool:
        # hammer /me many times concurrently across users
        jobs = sessions * 20
        for err in pool.map(check, jobs):
            if err:
                errors.append(err)

    if errors:
        raise SystemExit("concurrent /me failures:\n  " + "\n  ".join(errors[:10]))


def verify_rbac_create_user() -> None:
    # Non-superadmin must be forbidden
    for acct in ACCOUNTS:
        status, body = req(
            "POST",
            "/api/users",
            {
                "name": "Should Fail",
                "email": f"should.fail.{acct.role.lower()}@demo.local",
                "password": PASSWORD,
                "role": "SALES_EXECUTIVE",
            },
            token=acct.token,
        )
        if status != 403:
            raise SystemExit(
                f"{acct.role} create-user expected 403, got {status} {body}"
            )

    # Superadmin allowed
    status, body = login(SUPER_EMAIL, SUPER_PASSWORD)
    if status != 200:
        raise SystemExit(f"super login failed: {status}")
    super_token = body["token"]
    email = f"rbac.tmp.{int(time.time())}@demo.local"
    status, body = req(
        "POST",
        "/api/users",
        {
            "name": "RBAC Temp",
            "email": email,
            "password": PASSWORD,
            "role": "SUPPORT",
        },
        token=super_token,
    )
    if status not in (200, 201):
        raise SystemExit(f"superadmin create-user failed: {status} {body}")
    print(f"  superadmin create-user ok ({email})")


def verify_unauthenticated_blocked() -> None:
    status, _ = req("GET", "/api/auth/me")
    if status != 401:
        raise SystemExit(f"unauthenticated /me expected 401, got {status}")
    status, _ = req("GET", "/api/users")
    if status != 401:
        raise SystemExit(f"unauthenticated /users expected 401, got {status}")
    status, _ = req("GET", "/api/leads")
    if status != 401:
        raise SystemExit(f"unauthenticated /leads expected 401, got {status}")


def verify_cross_token_isolation() -> None:
    # Token A must never return user B
    a, b = ACCOUNTS[0], ACCOUNTS[1]
    status, body = req("GET", "/api/auth/me", token=a.token)
    if status != 200 or body["user"]["email"].lower() != a.email.lower():
        raise SystemExit("cross isolation failed for A")
    status, body = req("GET", "/api/auth/me", token=b.token)
    if status != 200 or body["user"]["email"].lower() != b.email.lower():
        raise SystemExit("cross isolation failed for B")
    # Tampered token rejected
    status, _ = req("GET", "/api/auth/me", token=a.token[:-4] + "xxxx")
    if status != 401:
        raise SystemExit(f"tampered token expected 401, got {status}")


def main() -> None:
    print(f"Target: {BASE}")
    status, health = req("GET", "/api/health")
    if status != 200:
        raise SystemExit(f"API unhealthy: {status} {health}")
    print(f"Health: {health.get('status')} / db={health.get('database')}")

    status, body = login(SUPER_EMAIL, SUPER_PASSWORD)
    if status != 200:
        raise SystemExit(f"superadmin login failed: {status} {body}")
    super_token = body["token"]
    print("Ensuring RBAC probe accounts…")
    ensure_accounts(super_token)

    print("Concurrent multi-user login…")
    results = concurrent_login(rounds=10)
    verify_identity_no_mix(results)
    print(f"  {len(results)} logins ok, identities isolated")

    print("Concurrent /me across roles…")
    concurrent_me_checks()
    print("  /me isolation ok")

    print("RBAC create-user…")
    verify_rbac_create_user()
    print("  non-superadmin forbidden, superadmin allowed")

    print("Unauthenticated + token isolation…")
    verify_unauthenticated_blocked()
    verify_cross_token_isolation()
    print("  protected routes + token isolation ok")

    print("\nPASS: concurrent auth + RBAC checks succeeded")


if __name__ == "__main__":
    main()
