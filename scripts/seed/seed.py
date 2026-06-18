#!/usr/bin/env python3
"""Comprehensive, idempotent demo seed for zk-drive.

Provisions a single canonical narrative used by every doc, blog post, and
screenshot in this repository:

  * Northwind Trading  — the primary tenant (an import/distribution SME):
    one owner-admin, a second admin, three members (one deactivated), folders
    spanning both privacy modes, a realistic file mix with real bytes, share
    links, a guest invite, a client room, a collaborative document, retention
    policies, a business billing plan, data-placement + customer-managed-key
    settings, an outbound webhook, and a KChat room mapping.
  * Lakeside Legal     — a second, fully isolated tenant used to demonstrate
    that one workspace can never see another's data.

Every action is a real authenticated API call against a running server, so
the audit log is genuinely hash-chained and the object store holds the real
uploaded bytes.

The script is dependency-free (Python standard library only) and idempotent:
re-running against an already-seeded database detects the existing primary
admin, re-derives the folder IDs, rewrites the state file, and exits without
creating duplicates.

Usage:
    BASE_URL=http://localhost:8080 \
    SEED_OUT=scripts/seed/out/state.json \
    python3 scripts/seed/seed.py

Outputs a state file (folder IDs, user IDs, sample tokens) that
frontend/e2e/blog-evidence.spec.ts reads to capture live screenshots.
"""

from __future__ import annotations

import io
import json
import os
import struct
import sys
import urllib.error
import urllib.request
import zipfile
import zlib

BASE_URL = os.environ.get("BASE_URL", "http://localhost:8080").rstrip("/")
SEED_OUT = os.environ.get(
    "SEED_OUT",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "out", "state.json"),
)
PASSWORD = os.environ.get("SEED_PASSWORD", "DemoPass!2026")


# --- HTTP helpers ---------------------------------------------------------


class APIError(Exception):
    def __init__(self, method, path, status, body):
        super().__init__(f"{method} {path} -> {status}: {body[:300]}")
        self.status = status
        self.body = body


def _request(method, url, token=None, body=None, raw=None, content_type=None):
    headers = {}
    data = None
    if raw is not None:
        data = raw
        if content_type:
            headers["Content-Type"] = content_type
    elif body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req) as resp:
            payload = resp.read()
            return resp.status, payload
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def api(method, path, token=None, body=None, expect=(200, 201)):
    status, payload = _request(method, BASE_URL + path, token=token, body=body)
    text = payload.decode("utf-8", "replace") if payload else ""
    if status not in expect:
        raise APIError(method, path, status, text)
    return json.loads(text) if text else {}


def try_api(method, path, token=None, body=None, expect=(200, 201)):
    """Best-effort variant: logs and swallows failures for optional steps so
    one non-essential 4xx never aborts the core seed."""
    try:
        return api(method, path, token=token, body=body, expect=expect)
    except APIError as e:
        print(f"   (skipped) {e}")
        return None


# --- File-byte generators (all standard-library) --------------------------


def _pdf_escape(s):
    return s.replace("\\", r"\\").replace("(", r"\(").replace(")", r"\)")


def make_pdf(title, lines):
    """A small but structurally valid single-page PDF with a real xref."""
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    pieces = [f"BT /F1 20 Tf 72 720 Td ({_pdf_escape(title)}) Tj ET"]
    y = 690
    for line in lines:
        pieces.append(f"BT /F1 11 Tf 72 {y} Td ({_pdf_escape(line)}) Tj ET")
        y -= 16
    content = "\n".join(pieces).encode("latin-1", "replace")
    objs.append(b"<< /Length %d >>\nstream\n" % len(content) + content + b"\nendstream")

    out = b"%PDF-1.4\n"
    offsets = []
    for i, obj in enumerate(objs, start=1):
        offsets.append(len(out))
        out += b"%d 0 obj\n" % i + obj + b"\nendobj\n"
    xref_pos = len(out)
    count = len(objs) + 1
    out += b"xref\n0 %d\n" % count
    out += b"0000000000 65535 f \n"
    for off in offsets:
        out += b"%010d 00000 n \n" % off
    out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (count, xref_pos)
    return out


def make_png(width, height, rgb):
    """A valid solid-colour 8-bit RGB PNG."""
    def chunk(typ, data):
        return (
            struct.pack(">I", len(data))
            + typ
            + data
            + struct.pack(">I", zlib.crc32(typ + data) & 0xFFFFFFFF)
        )

    sig = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    row = b"\x00" + bytes(rgb) * width
    idat = zlib.compress(row * height, 9)
    return sig + chunk(b"IHDR", ihdr) + chunk(b"IDAT", idat) + chunk(b"IEND", b"")


def _xml_escape(s):
    return (
        s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;").replace('"', "&quot;")
    )


def make_docx(title, paragraphs):
    ct = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
        '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>'
        '<Default Extension="xml" ContentType="application/xml"/>'
        '<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>'
        "</Types>"
    )
    rels = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
        '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>'
        "</Relationships>"
    )
    body = "".join(
        f'<w:p><w:r><w:t xml:space="preserve">{_xml_escape(p)}</w:t></w:r></w:p>'
        for p in [title, *paragraphs]
    )
    doc = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">'
        f"<w:body>{body}</w:body></w:document>"
    )
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("[Content_Types].xml", ct)
        z.writestr("_rels/.rels", rels)
        z.writestr("word/document.xml", doc)
    return buf.getvalue()


def _xlsx_col(index):
    """Excel column letters for a 0-based column index (0->A, 25->Z, 26->AA)."""
    letters = ""
    n = index + 1
    while n:
        n, rem = divmod(n - 1, 26)
        letters = chr(ord("A") + rem) + letters
    return letters


def make_xlsx(rows):
    ct = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
        '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>'
        '<Default Extension="xml" ContentType="application/xml"/>'
        '<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>'
        '<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>'
        "</Types>"
    )
    rels = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
        '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>'
        "</Relationships>"
    )
    workbook = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" '
        'xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">'
        '<sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>'
    )
    wb_rels = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
        '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>'
        "</Relationships>"
    )
    row_xml = []
    for ri, row in enumerate(rows, start=1):
        cells = []
        for ci, val in enumerate(row):
            ref = f"{_xlsx_col(ci)}{ri}"
            cells.append(
                f'<c r="{ref}" t="inlineStr"><is><t>{_xml_escape(str(val))}</t></is></c>'
            )
        row_xml.append(f'<row r="{ri}">{"".join(cells)}</row>')
    sheet = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">'
        f'<sheetData>{"".join(row_xml)}</sheetData></worksheet>'
    )
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("[Content_Types].xml", ct)
        z.writestr("_rels/.rels", rels)
        z.writestr("xl/workbook.xml", workbook)
        z.writestr("xl/_rels/workbook.xml.rels", wb_rels)
        z.writestr("xl/worksheets/sheet1.xml", sheet)
    return buf.getvalue()


# --- Domain operations ----------------------------------------------------


def signup(workspace_name, email, name):
    return api(
        "POST",
        "/api/auth/signup",
        body={
            "workspace_name": workspace_name,
            "email": email,
            "name": name,
            "password": PASSWORD,
        },
    )


def login(email):
    return api("POST", "/api/auth/login", body={"email": email, "password": PASSWORD})


def invite(token, email, name, role):
    return api(
        "POST",
        "/api/admin/users",
        token=token,
        body={"email": email, "name": name, "password": PASSWORD, "role": role},
    )


def create_folder(token, name, mode, parent=None):
    return api(
        "POST",
        "/api/folders",
        token=token,
        body={"name": name, "encryption_mode": mode, "parent_folder_id": parent},
    )


def upload(token, folder_id, name, mime, data):
    grant = api(
        "POST",
        "/api/files/upload-url",
        token=token,
        body={"folder_id": folder_id, "filename": name, "mime_type": mime},
    )
    status, _ = _request("PUT", grant["upload_url"], raw=data, content_type=mime)
    if status not in (200, 204):
        raise APIError("PUT", grant["upload_url"], status, "presigned upload failed")
    confirmed = api(
        "POST",
        "/api/files/confirm-upload",
        token=token,
        body={
            "file_id": grant["upload_id"],
            "object_key": grant["object_key"],
            "size_bytes": len(data),
        },
    )
    return confirmed["file"]


# --- File set for the Engineering / Finance / Marketing / Legal folders ----


def engineering_files():
    return [
        (
            "architecture-overview.pdf",
            "application/pdf",
            make_pdf(
                "Northwind Drive - Architecture Overview",
                [
                    "Single Go binary: HTTP API, WebSocket collaboration, and static SPA.",
                    "PostgreSQL for metadata; S3-compatible object storage for file bytes.",
                    "NATS JetStream drives post-upload jobs (scan, preview, search index).",
                    "Per-folder privacy: managed_encrypted vs strict_zk (end-to-end).",
                    "Audit log is HMAC hash-chained and independently verifiable.",
                ],
            ),
        ),
        (
            "api-spec-v3.yaml",
            "application/yaml",
            (
                "openapi: 3.1.0\n"
                "info:\n  title: Northwind Drive API\n  version: '3'\n"
                "paths:\n"
                "  /api/folders:\n    get: {summary: List folders}\n"
                "    post: {summary: Create folder}\n"
                "  /api/files/upload-url:\n    post: {summary: Mint presigned upload URL}\n"
            ).encode(),
        ),
        (
            "deployment-runbook.md",
            "text/markdown",
            (
                "# Deployment Runbook\n\n"
                "1. Apply migrations: `zkdrive migrate`.\n"
                "2. Start the server and at least one worker.\n"
                "3. Verify `/readyz` is green for postgres, object storage, and NATS.\n"
                "4. Roll forward one replica at a time; the audit chain stays intact.\n"
            ).encode(),
        ),
        (
            "load-test-results.csv",
            "text/csv",
            (
                "scenario,rps,p50_ms,p95_ms,p99_ms,error_rate\n"
                "browse_folders,820,11,34,58,0.000\n"
                "upload_10mb,140,180,420,690,0.001\n"
                "search_content,610,18,52,95,0.000\n"
                "audit_verify,75,240,510,880,0.000\n"
            ).encode(),
        ),
        (
            "security-audit-2026.pdf",
            "application/pdf",
            make_pdf(
                "Independent Security Review - 2026",
                [
                    "Scope: authentication, tenant isolation, encryption modes, audit chain.",
                    "Tenant isolation: no cross-workspace data access observed.",
                    "Audit chain: tamper-evident HMAC verified end to end.",
                    "Recommendations: enforce workspace MFA; rotate CMK annually.",
                ],
            ),
        ),
    ]


# --- Main seed flow -------------------------------------------------------


def seed_northwind():
    print("== Northwind Trading ==")
    auth = signup("Northwind Trading", "alice@northwind.example", "Alice Chen")
    admin = auth["token"]
    print("   owner-admin: alice@northwind.example")

    # Team: a second admin, two active members, one deactivated member.
    dave = invite(admin, "dave@northwind.example", "Dave Patel", "admin")
    bob = invite(admin, "bob@northwind.example", "Bob Martinez", "member")
    carol = invite(admin, "carol@northwind.example", "Carol Nguyen", "member")
    eve = invite(admin, "eve@northwind.example", "Eve Thompson", "member")
    api("DELETE", f"/api/admin/users/{eve['id']}", token=admin, expect=(200, 204))
    print("   team: +dave (admin) +bob +carol +eve (deactivated)")

    # Folders spanning both privacy modes.
    engineering = create_folder(admin, "Engineering", "managed_encrypted")
    architecture = create_folder(admin, "Architecture", "managed_encrypted", engineering["id"])
    releases = create_folder(admin, "Releases", "managed_encrypted", engineering["id"])
    finance = create_folder(admin, "Finance", "managed_encrypted")
    marketing = create_folder(admin, "Marketing Assets", "managed_encrypted")
    legal = create_folder(admin, "Legal Contracts", "strict_zk")
    vault = create_folder(admin, "Client Vault", "strict_zk")
    print("   folders: Engineering(+Architecture,+Releases), Finance, Marketing, Legal[zk], Vault[zk]")

    # Real files with real bytes.
    eng_first = None
    for name, mime, data in engineering_files():
        f = upload(admin, engineering["id"], name, mime, data)
        if eng_first is None:
            eng_first = f
    upload(
        admin,
        finance["id"],
        "q1-2026-budget.xlsx",
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        make_xlsx(
            [
                ["Department", "Budget", "Spent", "Remaining"],
                ["Engineering", 480000, 312500, 167500],
                ["Sales", 260000, 198400, 61600],
                ["Operations", 145000, 132100, 12900],
            ]
        ),
    )
    upload(
        admin,
        finance["id"],
        "vendor-invoices.csv",
        "text/csv",
        b"invoice,vendor,amount,status\nINV-1042,Acme Freight,12450.00,paid\nINV-1043,Pier 9 Logistics,8800.00,pending\n",
    )
    upload(
        admin,
        marketing["id"],
        "brand-guidelines.pdf",
        "application/pdf",
        make_pdf("Northwind Brand Guidelines", ["Primary indigo. Mona Sans + Sono.", "Pill buttons; generous spacing."]),
    )
    upload(admin, marketing["id"], "logo-primary.png", "image/png", make_png(480, 160, (85, 59, 216)))
    upload(
        admin,
        marketing["id"],
        "launch-plan.md",
        "text/markdown",
        b"# Launch Plan\n\n- Week 1: private beta\n- Week 3: public launch\n- Week 6: SME webinar series\n",
    )
    # strict_zk folders: the server stores only ciphertext, so these are
    # opaque blobs as far as preview/search/scan are concerned.
    upload(
        admin,
        legal["id"],
        "msa-northwind-2026.pdf",
        "application/pdf",
        make_pdf("Master Services Agreement (CONFIDENTIAL)", ["End-to-end encrypted. Server holds ciphertext only."]),
    )
    upload(
        admin,
        legal["id"],
        "nda-template.docx",
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        make_docx("Mutual NDA", ["This template is encrypted client-side before upload."]),
    )
    upload(
        admin,
        vault["id"],
        "onboarding-packet.pdf",
        "application/pdf",
        make_pdf("Client Onboarding Packet", ["Shared with the client via a guest invite."]),
    )
    print("   files: uploaded real bytes across all folders")

    # A collaborative document (live-presence) in a managed folder.
    try_api(
        "POST",
        "/api/documents",
        token=admin,
        body={"folder_id": engineering["id"], "name": "Q2 Planning Notes", "collab_mode": "rich_presence"},
    )

    # Sharing: a password + expiry + download-capped link on a real file,
    # plus a folder share link, plus a guest invite into the client vault.
    if eng_first:
        try_api(
            "POST",
            "/api/share-links",
            token=admin,
            body={
                "resource_type": "file",
                "resource_id": eng_first["id"],
                "role": "viewer",
                "password": "Architecture#2026",
                "expires_at": "2026-12-31T23:59:59Z",
                "max_downloads": 25,
            },
        )
    try_api(
        "POST",
        "/api/share-links",
        token=admin,
        body={"resource_type": "folder", "resource_id": marketing["id"], "role": "viewer"},
    )
    try_api(
        "POST",
        "/api/guest-invites",
        token=admin,
        body={"folder_id": vault["id"], "email": "client@brightwave-partners.example", "role": "viewer"},
    )

    # A client room from a template (provisions a sub-folder tree + link).
    templates = try_api("GET", "/api/client-rooms/templates", token=admin)
    if templates and templates.get("templates"):
        tmpl = templates["templates"][0]["name"]
        try_api(
            "POST",
            "/api/client-rooms/from-template",
            token=admin,
            body={"template": tmpl, "name": "Brightwave Partners"},
        )

    # Retention: a workspace default plus a stricter policy on Legal.
    try_api(
        "POST",
        "/api/admin/retention-policies",
        token=admin,
        body={"folder_id": None, "max_versions": 10, "archive_after_days": 365},
    )
    try_api(
        "POST",
        "/api/admin/retention-policies",
        token=admin,
        body={"folder_id": legal["id"], "max_versions": 25},
    )

    # Billing: put the workspace on the business tier with explicit limits.
    try_api(
        "PUT",
        "/api/admin/billing/plan",
        token=admin,
        body={
            "tier": "business",
            "max_storage_bytes": 1099511627776,
            "max_users": 50,
            "max_bandwidth_bytes_monthly": 5497558138880,
        },
    )

    # Data placement (round-trip the GET payload with edits) + CMK + default mode.
    placement = try_api("GET", "/api/admin/placement", token=admin)
    if placement is not None:
        placement.setdefault("policy", {})
        placement["policy"]["placement"] = {
            "provider": ["aws"],
            "region": ["us-east-1"],
            "country": ["US"],
            "storage_class": ["STANDARD"],
        }
        placement["policy"].setdefault("encryption", {})
        placement["policy"]["encryption"]["mode"] = "managed"
        try_api("PUT", "/api/admin/placement", token=admin, body=placement)
    try_api(
        "PUT",
        "/api/admin/cmk",
        token=admin,
        body={"cmk_uri": "arn:aws:kms:us-east-1:000000000000:key/northwind-demo-cmk"},
    )
    try_api(
        "PUT",
        "/api/admin/workspace/default-encryption-mode",
        token=admin,
        body={"mode": "managed_encrypted"},
    )

    # An outbound webhook subscription (one per event type).
    try_api(
        "POST",
        "/api/admin/webhooks",
        token=admin,
        body={
            "url": "https://example.com/webhooks/zk-drive",
            "event_type": "file.upload.confirmed",
            "description": "Notify the data warehouse of new uploads",
        },
    )

    # A KChat room mapping (auto-provisions its backing folder).
    try_api("POST", "/api/kchat/rooms", token=admin, body={"kchat_room_id": "northwind-engineering"})

    # Mark guided setup complete so the main app renders without the wizard gate.
    try_api("POST", "/api/setup/complete", token=admin, expect=(200, 204))

    return {
        "workspace": auth["workspace_id"],
        "users": {
            "alice": auth["user_id"],
            "dave": dave["id"],
            "bob": bob["id"],
            "carol": carol["id"],
            "eve": eve["id"],
        },
        "folders": {
            "engineering": engineering["id"],
            "architecture": architecture["id"],
            "releases": releases["id"],
            "finance": finance["id"],
            "marketing": marketing["id"],
            "legal": legal["id"],
            "vault": vault["id"],
        },
    }


def seed_lakeside():
    print("== Lakeside Legal (isolation tenant) ==")
    auth = signup("Lakeside Legal", "morgan@lakeside.example", "Morgan Reyes")
    token = auth["token"]
    matters = create_folder(token, "Client Matters", "strict_zk")
    briefs = create_folder(token, "Case Briefs", "managed_encrypted")
    upload(
        token,
        briefs["id"],
        "intake-checklist.pdf",
        "application/pdf",
        make_pdf("Lakeside Legal - Intake Checklist", ["Separate tenant. Northwind can never see this."]),
    )
    try_api("POST", "/api/setup/complete", token=token, expect=(200, 204))
    print("   owner-admin: morgan@lakeside.example; folders: Client Matters[zk], Case Briefs")
    return {"workspace": auth["workspace_id"], "folders": {"matters": matters["id"], "briefs": briefs["id"]}}


def derive_existing(auth):
    """Idempotent path: primary admin already exists. Re-derive the FULL
    state (users, root folders, Engineering subfolders, and the Lakeside
    isolation tenant) from the live workspace so a re-run reproduces the
    exact same state file a fresh seed would, without duplicating data.

    `auth` is the already-parsed login response from main()'s idempotency
    probe, reused here so the re-run authenticates the primary admin once."""
    print("Primary admin already exists — re-deriving state (idempotent re-run).")
    token = auth["token"]

    users = {u["email"]: u["id"] for u in api("GET", "/api/admin/users", token=token).get("users", [])}
    roots = {f["name"]: f["id"] for f in api("GET", "/api/folders?parent_folder_id=root", token=token).get("folders", [])}
    engineering = roots.get("Engineering", "")
    children = {}
    if engineering:
        kids = api("GET", f"/api/folders?parent_folder_id={engineering}", token=token).get("folders", [])
        children = {c["name"]: c["id"] for c in kids}

    primary = {
        "workspace": auth["workspace_id"],
        "users": {
            "alice": users.get("alice@northwind.example", auth["user_id"]),
            "dave": users.get("dave@northwind.example", ""),
            "bob": users.get("bob@northwind.example", ""),
            "carol": users.get("carol@northwind.example", ""),
            "eve": users.get("eve@northwind.example", ""),
        },
        "folders": {
            "engineering": engineering,
            "architecture": children.get("Architecture", ""),
            "releases": children.get("Releases", ""),
            "finance": roots.get("Finance", ""),
            "marketing": roots.get("Marketing Assets", ""),
            "legal": roots.get("Legal Contracts", ""),
            "vault": roots.get("Client Vault", ""),
        },
    }
    state = {"primary": primary}

    # Lakeside isolation tenant — only present if it was seeded. Best-effort
    # so the idempotent re-run still succeeds against partial deployments.
    lstatus, lpayload = _request("POST", BASE_URL + "/api/auth/login", body={"email": "morgan@lakeside.example", "password": PASSWORD})
    if lstatus == 200:
        lauth = json.loads(lpayload.decode("utf-8"))
        lroots = {f["name"]: f["id"] for f in api("GET", "/api/folders?parent_folder_id=root", token=lauth["token"]).get("folders", [])}
        state["isolation"] = {
            "workspace": lauth["workspace_id"],
            "folders": {"matters": lroots.get("Client Matters", ""), "briefs": lroots.get("Case Briefs", "")},
        }
    return state


def main():
    print(f"Seeding zk-drive at {BASE_URL}")
    # Idempotency guard: if the primary admin already exists, don't re-create.
    status, payload = _request("POST", BASE_URL + "/api/auth/login", body={"email": "alice@northwind.example", "password": PASSWORD})
    if status == 200:
        state = derive_existing(json.loads(payload.decode("utf-8")))
    else:
        northwind = seed_northwind()
        lakeside = seed_lakeside()
        state = {"primary": northwind, "isolation": lakeside}

    # Flatten the primary folders to the top level for the e2e spec, which
    # reads seed.folders.{engineering,legal,vault}.
    state["folders"] = state["primary"]["folders"]

    os.makedirs(os.path.dirname(SEED_OUT), exist_ok=True)
    with open(SEED_OUT, "w", encoding="utf-8") as fh:
        json.dump(state, fh, indent=2)
    print(f"\nWrote state to {SEED_OUT}")
    print(json.dumps(state["folders"], indent=2))


if __name__ == "__main__":
    try:
        main()
    except APIError as e:
        print(f"\nSEED FAILED: {e}", file=sys.stderr)
        sys.exit(1)
    except (urllib.error.URLError, OSError) as e:
        print(
            f"\nSEED FAILED: cannot reach the server at {BASE_URL} ({e}). "
            "Is the local stack running?",
            file=sys.stderr,
        )
        sys.exit(1)
    except Exception as e:  # friendly catch-all for a developer-facing tool
        print(f"\nSEED FAILED: unexpected error: {e!r}", file=sys.stderr)
        sys.exit(1)
