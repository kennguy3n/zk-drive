import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  fetchPlacement,
  updatePlacement,
  type PlacementPolicy,
} from "../api/client";
import { useAuth } from "../hooks/useAuth";

// PlacementPage lets workspace admins view and edit the data-residency
// placement policy that zk-object-fabric uses to route per-workspace
// storage. The form exposes the subset of fabric.Policy the UI cares
// about; other fields (tenant, cache location) are preserved from the
// GET payload when we PUT.
export default function PlacementPage() {
  const { isAdmin } = useAuth();
  const [policy, setPolicy] = useState<PlacementPolicy | null>(null);
  const [provider, setProvider] = useState<string>("wasabi");
  const [region, setRegion] = useState("");
  const [country, setCountry] = useState("");
  const [storageClass, setStorageClass] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setError(null);
    setLoading(true);
    try {
      const p = await fetchPlacement();
      setPolicy(p);
      const pl = p.policy.placement;
      setProvider(pl.provider?.[0] ?? "wasabi");
      setRegion(pl.region?.[0] ?? "");
      setCountry(pl.country?.[0] ?? "");
      setStorageClass(pl.storage_class?.[0] ?? "");
    } catch (e) {
      setError(errMessage(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (isAdmin) load();
  }, [isAdmin, load]);

  if (!isAdmin) {
    return (
      <div style={{ padding: 32 }}>
        <h2>Admin only</h2>
        <p>
          This page is restricted to workspace administrators.{" "}
          <Link to="/drive">Back to drive</Link>
        </p>
      </div>
    );
  }

  const save = async () => {
    setError(null);
    setMessage(null);
    if (!policy) return;
    // Preserve every field from the GET payload; we only replace the
    // slices the form edits. This keeps tenant / encryption / cache
    // location stable across a PUT.
    const next: PlacementPolicy = {
      ...policy,
      policy: {
        ...policy.policy,
        placement: {
          ...policy.policy.placement,
          provider: provider ? [provider] : [],
          region: region ? [region] : [],
          country: country ? [country.trim().toUpperCase()] : [],
          storage_class: storageClass ? [storageClass] : [],
        },
      },
    };
    try {
      await updatePlacement(next);
      setMessage("Placement policy saved.");
      await load();
    } catch (e) {
      setError(errMessage(e));
    }
  };

  return (
    <div style={{ padding: 24 }}>
      <header
        style={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          marginBottom: 16,
        }}
      >
        <h1 style={{ margin: 0 }}>Placement policy</h1>
        <Link to="/admin">Back to admin</Link>
      </header>
      {loading ? <p>Loading…</p> : null}
      {error ? <p style={{ color: "#b91c1c" }}>{error}</p> : null}
      {message ? <p style={{ color: "#047857" }}>{message}</p> : null}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
        style={{ display: "grid", gap: 12, maxWidth: 480 }}
      >
        <label style={{ display: "grid", gap: 4 }}>
          <span>Provider</span>
          <select value={provider} onChange={(e) => setProvider(e.target.value)}>
            <option value="wasabi">wasabi</option>
            <option value="b2">b2</option>
            <option value="s3">s3</option>
          </select>
        </label>
        <label style={{ display: "grid", gap: 4 }}>
          <span>Region</span>
          <input
            value={region}
            onChange={(e) => setRegion(e.target.value)}
            placeholder="e.g. us-east-1"
          />
        </label>
        <label style={{ display: "grid", gap: 4 }}>
          <span>Country (ISO-3166 alpha-2)</span>
          <input
            value={country}
            onChange={(e) => setCountry(e.target.value)}
            placeholder="e.g. US, DE"
            maxLength={2}
          />
        </label>
        <label style={{ display: "grid", gap: 4 }}>
          <span>Storage class</span>
          <input
            value={storageClass}
            onChange={(e) => setStorageClass(e.target.value)}
            placeholder="e.g. hot, cold"
          />
        </label>
        <div style={{ display: "flex", gap: 8 }}>
          <button type="submit">Save</button>
          <button type="button" onClick={load}>
            Reset
          </button>
        </div>
      </form>
    </div>
  );
}

function errMessage(e: unknown): string {
  if (e && typeof e === "object" && "message" in e) {
    return String((e as { message: unknown }).message);
  }
  return String(e);
}
