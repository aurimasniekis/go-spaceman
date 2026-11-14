import subprocess, sys, os, shutil, importlib.util
root = sys.argv[1]
spec = importlib.util.spec_from_file_location("m", sys.argv[2]); m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
only = set(sys.argv[3:])
pkgs = ["./internal/api/", "./internal/auth/", "./internal/app/", "./internal/store/", "./internal/telegram/"]
pat = "Telegram|InProcess|Actor|PrincipalSubjects|StreamWatch|Approver|AutoExcluded"
env = dict(os.environ, SM_TEST_TX_MAX="500ms", GOFLAGS="-p=2 -buildvcs=false")
res = []
for mid, f, old, new in m.M:
    if only and mid not in only: continue
    p = os.path.join(root, f)
    src = open(p).read()
    n = src.count(old)
    if n != 1:
        print(f"{mid}: BAD PATTERN (count {n})", flush=True); res.append((mid, "bad")); continue
    shutil.copy(p, p + ".bak")
    try:
        open(p, "w").write(src.replace(old, new))
        r = subprocess.run(["timeout", "600", "go", "test", "-count=1", "-run", pat] + pkgs, cwd=root, env=env, capture_output=True, text=True)
        out = r.stdout + r.stderr
        if "[build failed]" in out or "setup failed" in out:
            verdict = "BUILD-FAILED"
        elif r.returncode == 0:
            verdict = "SURVIVED"
        else:
            verdict = "killed"
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        succeeded = "SUCCEEDED" in out
        if succeeded:
            print("  SUCCEEDED lines:", " | ".join(l.strip()[:160] for l in out.splitlines() if "SUCCEEDED" in l)[:2000], flush=True)
        print(f"{mid}: {verdict} {' '.join(fails)[:300]}{' [a refused op SUCCEEDED]' if succeeded else ''}", flush=True)
        if mid == "refusal-revert":
            print("  refusal-revert detail:", "\n  ".join(l for l in out.splitlines() if "recent authentication" in l or "re-authentication" in l or "mfa_required" in l)[:1500], flush=True)
        res.append((mid, verdict))
    finally:
        shutil.move(p + ".bak", p)
print("SUMMARY", {v: sum(1 for _, x in res if x == v) for v in set(x for _, x in res)})
