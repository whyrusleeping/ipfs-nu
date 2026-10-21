#!/usr/bin/env python3
"""Bounded, exhaustive main-module fuzz campaign (never a per-PR test).

Go writes minimized failures to <package>/testdata/fuzz/<target>/ automatically.
Review and commit those files as regression seeds; CI uploads them on failure.
"""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--time', default='10m', help='Go fuzz duration per target')
    p.add_argument('--jobs', type=int, default=4)
    p.add_argument('--out', type=Path, default=Path('.hardening/fuzz'))
    args = p.parse_args()
    if args.jobs < 1 or not re.fullmatch(r'[1-9][0-9]*(s|m|h)', args.time):
        p.error('positive jobs and duration in whole seconds/minutes/hours required')
    seconds = int(args.time[:-1]) * {'s': 1, 'm': 60, 'h': 3600}[args.time[-1]]
    os.chdir(Path(__file__).resolve().parent.parent)
    args.out.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, CGO_ENABLED='0', GOMAXPROCS='2')
    listing = subprocess.check_output(
        ['go', 'test', '-json', '-list', '^Fuzz', './...'], env=env, text=True)
    (args.out / 'discovery.jsonl').write_text(listing)
    targets = []
    for line in listing.splitlines():
        event = json.loads(line)
        name = event.get('Output', '').strip()
        if re.fullmatch(r'Fuzz\w+', name):
            targets.append((event['Package'], name))
    if not targets:
        raise SystemExit('No fuzz targets discovered')
    print(f'{len(targets)} targets, {args.time} each, {args.jobs} concurrent', flush=True)

    def run(target):
        package, name = target
        log = package.replace('/', '_') + '.' + name + '.log'
        cmd = ['go', 'test', package, '-run', '^$', '-fuzz', '^' + name + '$',
               '-fuzztime', args.time, '-parallel', '2', '-timeout', f'{seconds + 180}s']
        start = time.monotonic()
        with (args.out / log).open('w') as out:
            proc = subprocess.Popen(cmd, env=env, stdout=out, stderr=subprocess.STDOUT,
                                    start_new_session=True)
            try:
                rc = proc.wait(timeout=seconds + 240)
            except subprocess.TimeoutExpired:
                # Kill the test binary and fuzz workers too, not just `go test`.
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
                rc = 124
        result = dict(package=package, target=name, exit=rc,
                      seconds=round(time.monotonic() - start, 3), log=log)
        print(json.dumps(result), flush=True)
        return result

    with concurrent.futures.ThreadPoolExecutor(max_workers=args.jobs) as pool:
        results = list(pool.map(run, targets))
    (args.out / 'summary.json').write_text(json.dumps(results, indent=2) + '\n')
    raise SystemExit(int(any(r['exit'] != 0 for r in results)))


if __name__ == '__main__':
    main()
