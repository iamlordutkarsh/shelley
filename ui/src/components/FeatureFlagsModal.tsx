import React, { useState, useEffect, useCallback } from "react";
import Modal from "./Modal";
import { featureFlagsApi, type FeatureFlag } from "../services/api";

interface FeatureFlagsModalProps {
  isOpen: boolean;
  onClose: () => void;
}

// Editor for a single flag. Keeps a draft string while editing; commits on
// blur / Save. Booleans get a checkbox; everything else gets a JSON textarea.
function FlagRow({
  flag,
  onSave,
  onClear,
}: {
  flag: FeatureFlag;
  onSave: (name: string, value: unknown) => Promise<void>;
  onClear: (name: string) => Promise<void>;
}) {
  const effective = flag.override !== undefined ? flag.override : flag.default;
  const overridden = flag.override !== undefined;
  const isBool = typeof effective === "boolean" || typeof flag.default === "boolean";

  const [draft, setDraft] = useState<string>(JSON.stringify(effective, null, 2));
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Reset draft when the underlying flag changes (after save/refresh).
  useEffect(() => {
    setDraft(JSON.stringify(effective, null, 2));
    setError(null);
  }, [JSON.stringify(effective)]);

  const commitJSON = async () => {
    setError(null);
    let parsed: unknown;
    try {
      parsed = JSON.parse(draft);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Invalid JSON");
      return;
    }
    try {
      setBusy(true);
      await onSave(flag.name, parsed);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Save failed");
    } finally {
      setBusy(false);
    }
  };

  const toggleBool = async (next: boolean) => {
    setError(null);
    setBusy(true);
    try {
      await onSave(flag.name, next);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Save failed");
    } finally {
      setBusy(false);
    }
  };

  const clear = async () => {
    setError(null);
    setBusy(true);
    try {
      await onClear(flag.name);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Clear failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="feature-flag-row">
      <div className="feature-flag-head">
        <code className="feature-flag-name">{flag.name}</code>
        {overridden && <span className="feature-flag-badge">overridden</span>}
      </div>
      {flag.description && <div className="feature-flag-desc">{flag.description}</div>}

      {isBool ? (
        <label className="feature-flag-bool">
          <input
            type="checkbox"
            checked={effective === true}
            disabled={busy}
            onChange={(e) => toggleBool(e.target.checked)}
          />
          <span>{effective === true ? "true" : "false"}</span>
        </label>
      ) : (
        <>
          <textarea
            className="form-input feature-flag-json"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            spellCheck={false}
            rows={Math.min(8, draft.split("\n").length)}
            disabled={busy}
          />
          <div className="feature-flag-actions">
            <button
              className="btn-primary"
              onClick={commitJSON}
              disabled={busy || draft === JSON.stringify(effective, null, 2)}
            >
              Save
            </button>
          </div>
        </>
      )}

      <div className="feature-flag-meta">
        <span>
          default: <code>{JSON.stringify(flag.default)}</code>
        </span>
        {overridden && (
          <button className="btn-secondary feature-flag-clear" onClick={clear} disabled={busy}>
            Reset to default
          </button>
        )}
      </div>

      {error && <div className="feature-flag-error">{error}</div>}
    </div>
  );
}

function FeatureFlagsModal({ isOpen, onClose }: FeatureFlagsModalProps) {
  const [flags, setFlags] = useState<FeatureFlag[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setFlags(await featureFlagsApi.list());
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load feature flags");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (isOpen) load();
  }, [isOpen, load]);

  const handleSave = useCallback(
    async (name: string, value: unknown) => {
      await featureFlagsApi.set(name, value);
      await load();
    },
    [load],
  );

  const handleClear = useCallback(
    async (name: string) => {
      await featureFlagsApi.clear(name);
      await load();
    },
    [load],
  );

  return (
    <Modal isOpen={isOpen} onClose={onClose} title="Feature flags">
      {loading && <div>Loading…</div>}
      {error && <div className="feature-flag-error">{error}</div>}
      {!loading && !error && flags.length === 0 && (
        <div className="feature-flag-empty">
          No feature flags are defined. Add some by calling <code>featureflags.Register</code> in
          the Go code.
        </div>
      )}
      <div className="feature-flag-list">
        {flags.map((f) => (
          <FlagRow key={f.name} flag={f} onSave={handleSave} onClear={handleClear} />
        ))}
      </div>
    </Modal>
  );
}

export default FeatureFlagsModal;
