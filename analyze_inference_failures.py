import json
import os
import subprocess
import resource

with open("tools/benchmark/data/maven_top_500.json") as f:
    data = json.load(f)

output_dir = "go_infer_logs"
os.makedirs(output_dir, exist_ok=True)

MAX_VIRTUAL_MEMORY = 5 * 1024 * 1024 * 1024 # 5 GB

def limit_virtual_memory():
    # The tuple below is of the form (soft limit, hard limit). Limit only
    # the soft part so that the limit can be increased later (setting also
    # the hard limit would prevent that).
    # When the limit cannot be changed, setrlimit() raises ValueError.
    resource.setrlimit(resource.RLIMIT_AS, (MAX_VIRTUAL_MEMORY, resource.RLIM_INFINITY))

for pkg in data["Packages"]:
    name = pkg["Name"]
    version = pkg["Versions"][0]
    artifact = pkg["Artifacts"][0]
    cmd = [
        "go", "run", "tools/ctl/ctl.go",
        "infer",
        "--ecosystem=maven",
        f"--package={name}",
        f"--version={version}",
        f"--artifact={artifact}"
    ]
    log_file = os.path.join(output_dir, f"{name.replace(':', '_')}_{version}.log")
    proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, preexec_fn=limit_virtual_memory)
    if proc.returncode != 0:
        with open(log_file, "w") as logf:
            logf.write("COMMAND: " + " ".join(cmd) + "\n\n")
            logf.write(proc.stdout.decode())
            print(f"Exit code {proc.returncode} for {name} {version}, log: {log_file}")
