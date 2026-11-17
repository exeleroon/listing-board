import { FormEvent, useCallback, useEffect, useState } from "react";

type Step = { label: string; state: string };

type Job = {
  id: string;
  title: string;
  address: string;
  status: string;
  error: string;
  runId: string;
  filename: string;
  checklist: Step[];
};

type Account = { id: string; email: string };

// How often the open sheet is re-read while the worker runs it.
const LIVE_SHEET_INTERVAL_MS = 1000;

// Mirrors the API limit so an oversized file is refused before it is sent.
const MAX_BROCHURE_BYTES = 1024 * 1024;

class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: "include", ...init });
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) {
    const message = data && typeof data.error === "string" ? data.error : res.statusText;
    throw new ApiError(message, res.status);
  }
  return data as T;
}

function getJob(id: string): Promise<Job> {
  return api<Job>("/api/jobs/" + id);
}

function startJob(id: string): Promise<Job> {
  return api<Job>("/api/jobs/" + id + "/start", { method: "POST" });
}

function uploadBrochure(id: string, file: File): Promise<Job> {
  if (file.size > MAX_BROCHURE_BYTES) {
    return Promise.reject(new Error("Brochure is too large, the limit is 1 MiB"));
  }
  const body = new FormData();
  body.append("file", file);
  return api<Job>("/api/jobs/" + id + "/file", { method: "POST", body });
}

function describe(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

export function App() {
  const [me, setMe] = useState<Account | null>(null);
  const [ready, setReady] = useState(false);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [selected, setSelected] = useState<Job | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    api<Account>("/api/me")
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setReady(true));
  }, []);

  useEffect(() => {
    if (!me) {
      setJobs([]);
      return;
    }
    api<Job[]>("/api/jobs")
      .then(setJobs)
      .catch((err: Error) => setError(err.message));
  }, [me]);

  // Single place that puts a fresh copy of a sheet into the list and opens it.
  const showJob = useCallback((job: Job) => {
    setSelected(job);
    setJobs((current) =>
      current.some((item) => item.id === job.id)
        ? current.map((item) => (item.id === job.id ? job : item))
        : [job, ...current],
    );
  }, []);

  if (!ready) {
    return <p className="muted">Loading…</p>;
  }
  if (!me) {
    return <Auth onIn={setMe} />;
  }

  return (
    <main>
      <header>
        <div>
          <p className="eyebrow">Real estate</p>
          <h1>Listing board</h1>
        </div>
        <button
          className="ghost"
          onClick={() => {
            api("/api/logout", { method: "POST" }).finally(() => {
              setMe(null);
              setSelected(null);
              setError("");
            });
          }}
        >
          Log out
        </button>
      </header>
      {/* Always in the layout, so a message appearing does not push the page down. */}
      <p className="notice error" role="status">
        {error}
      </p>
      <Create
        onSaved={(job) => {
          showJob(job);
          setError("");
        }}
        onError={setError}
      />
      <section className="layout">
        <ul className="list">
          {jobs.length === 0 ? <li className="muted">No sheets yet.</li> : null}
          {jobs.map((job) => (
            <li key={job.id}>
              <button
                className={selected?.id === job.id ? "row on" : "row"}
                onClick={() => {
                  // An error belongs to the action that caused it, not to the next sheet.
                  setError("");
                  setSelected(job);
                }}
              >
                <span>
                  <strong>{job.title}</strong>
                  <span className="muted">{job.address}</span>
                </span>
                <span className={"status " + job.status}>{job.status}</span>
              </button>
            </li>
          ))}
        </ul>
        {selected ? <Sheet job={selected} onChange={showJob} onError={setError} /> : <p className="muted">Select a sheet.</p>}
      </section>
    </main>
  );
}

function Auth({ onIn }: { onIn: (account: Account) => void }) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    try {
      const account = await api<Account>("/api/" + mode, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, password }),
      });
      onIn(account);
    } catch (err) {
      setError(describe(err, "Could not sign in"));
    }
  }

  return (
    <main className="narrow">
      <p className="eyebrow">Real estate</p>
      <h1>Listing board</h1>
      <form onSubmit={submit}>
        <label>
          Email
          <input value={email} onChange={(event) => setEmail(event.target.value)} type="email" required />
        </label>
        <label>
          Password
          <input value={password} onChange={(event) => setPassword(event.target.value)} type="password" required />
        </label>
        {error ? <p className="error">{error}</p> : null}
        <button type="submit">{mode === "login" ? "Sign in" : "Create account"}</button>
      </form>
      <button className="ghost" onClick={() => setMode(mode === "login" ? "register" : "login")}>
        {mode === "login" ? "Need an account?" : "Already have an account?"}
      </button>
    </main>
  );
}

function Create({ onSaved, onError }: { onSaved: (job: Job) => void; onError: (message: string) => void }) {
  const [title, setTitle] = useState("");
  const [address, setAddress] = useState("");
  const [file, setFile] = useState<File | null>(null);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    onError("");
    if (!file) {
      onError("Choose a brochure file");
      return;
    }
    // Captured before the first await: React drops currentTarget afterwards.
    const form = event.currentTarget;
    let job: Job;
    try {
      job = await api<Job>("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title, address }),
      });
    } catch (err) {
      onError(describe(err, "Could not create the sheet"));
      return;
    }
    // The sheet exists now; show it even if the brochure is rejected, so the
    // user can attach another one from the sheet panel.
    onSaved(job);
    setTitle("");
    setAddress("");
    setFile(null);
    // The file input is uncontrolled; without this it keeps showing the old name.
    form.reset();
    try {
      onSaved(await uploadBrochure(job.id, file));
    } catch (err) {
      onError(describe(err, "Could not store the brochure"));
    }
  }

  return (
    <form className="create" onSubmit={submit}>
      <label>
        Title
        <input value={title} onChange={(event) => setTitle(event.target.value)} required />
      </label>
      <label>
        Address
        <input value={address} onChange={(event) => setAddress(event.target.value)} required />
      </label>
      <label>
        Brochure
        <input
          type="file"
          accept=".pdf,.txt,application/pdf,text/plain"
          onChange={(event) => {
            onError("");
            setFile(event.target.files?.[0] ?? null);
          }}
          required
        />
      </label>
      <button type="submit">Create sheet</button>
    </form>
  );
}

// Re-reads the sheet while the worker runs it, so the checklist moves without
// a reload. The first read happens at once, so a sheet opened from a stale
// list row is corrected immediately. Each later read is scheduled only after
// the previous one answered, so a slow response can never land after a newer
// one and move the checklist back. Stops as soon as the sheet leaves running,
// another one is opened, or the session is gone.
function useLiveSheet(job: Job, onChange: (job: Job) => void, onError: (message: string) => void) {
  useEffect(() => {
    if (job.status !== "running") {
      return;
    }
    let active = true;
    let timer = 0;
    let failing = false;
    const tick = async () => {
      try {
        const next = await getJob(job.id);
        if (!active) {
          return;
        }
        if (failing) {
          failing = false;
          onError("");
        }
        onChange(next);
      } catch (err) {
        if (!active) {
          return;
        }
        failing = true;
        onError(describe(err, "Could not refresh the sheet"));
        if (err instanceof ApiError && err.status === 401) {
          return;
        }
      }
      timer = window.setTimeout(tick, LIVE_SHEET_INTERVAL_MS);
    };
    timer = window.setTimeout(tick, 0);
    return () => {
      active = false;
      window.clearTimeout(timer);
    };
  }, [job.id, job.status, onChange, onError]);
}

function Sheet({
  job,
  onChange,
  onError,
}: {
  job: Job;
  onChange: (job: Job) => void;
  onError: (message: string) => void;
}) {
  useLiveSheet(job, onChange, onError);

  async function start() {
    onError("");
    try {
      onChange(await startJob(job.id));
    } catch (err) {
      onError(describe(err, "Could not start the sheet"));
    }
  }

  async function attach(file: File) {
    onError("");
    try {
      onChange(await uploadBrochure(job.id, file));
    } catch (err) {
      onError(describe(err, "Could not store the brochure"));
    }
  }

  return (
    <article className="sheet">
      <h2>{job.title}</h2>
      <p>{job.address}</p>
      <p className={"status " + job.status}>{job.status}</p>
      {job.filename ? (
        <p className="muted">
          <a href={"/api/jobs/" + job.id + "/file"}>{job.filename}</a>
        </p>
      ) : null}
      {job.runId ? <p className="muted">run {job.runId}</p> : null}
      {job.error ? <p className="error">{job.error}</p> : null}
      <ol>
        {job.checklist.map((step) => (
          <li key={step.label} className={step.state}>
            {step.label}
          </li>
        ))}
      </ol>
      {job.status === "draft" && !job.filename ? (
        <label>
          Attach a brochure
          <input
            type="file"
            accept=".pdf,.txt,application/pdf,text/plain"
            onChange={(event) => {
              const file = event.target.files?.[0];
              if (file) {
                void attach(file);
              }
            }}
          />
        </label>
      ) : null}
      {job.status === "draft" ? <button onClick={start}>Start</button> : null}
    </article>
  );
}
