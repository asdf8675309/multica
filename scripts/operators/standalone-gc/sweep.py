#!/usr/bin/env python3
"""Sweep configured standalone task worktrees; dry-run unless --apply is specified."""
import argparse
import datetime
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import time

from dataclasses import dataclass

@dataclass(frozen=True)
class Settings:
    project_id: str
    prefix: str
    roots: tuple
    daemon_config: Path
    lock_file: Path
    remote: str = 'origin'
    branch: str = 'main'

def load_settings(path):
    data = json.loads(Path(path).read_text())
    project = data['project_id']
    import uuid
    uuid.UUID(project)
    prefix = data['card_prefix']
    if not re.fullmatch(r'[A-Za-z][A-Za-z0-9_]*', prefix):
        raise ValueError('Invalid card prefix')
    roots = []
    for root in data['roots']:
        p = Path(root['path']).expanduser()
        if not p.is_absolute() or p.is_symlink() or p.resolve() != p:
            raise ValueError('Roots must be absolute canonical directories')
        allow = root.get('allow_number_only', False)
        if not isinstance(allow, bool):
            raise ValueError('allow_number_only must be boolean')
        roots.append((p, allow))
    if not roots or len({p for p, _ in roots}) != len(roots):
        raise ValueError('Specify unique discovery roots')
    remote, branch = data.get('remote', 'origin'), data.get('branch', 'main')
    if not all(re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_./-]*', value) for value in (remote, branch)):
        raise ValueError('Invalid remote or branch')
    return Settings(project, prefix, tuple(roots),
                    Path(data['daemon_config']).expanduser(),
                    Path(data['lock_file']).expanduser(), remote, branch)

def candidate_number(name, allow_number_only=False, prefix="MDT"):
    if allow_number_only and re.fullmatch(r'wt-\d+', name, re.I):
        return int(name.split('-')[1])
    if 'wt' not in name.lower():
        return None
    matches = re.findall(re.escape(prefix) + r'[-_]?(\d+)(?!\d)', name, re.I)
    return int(matches[0]) if len(matches) == 1 else None

def duration(value):
    parts = re.findall(r'(\d+(?:\.\d+)?)([smh])', value)
    if not parts or ''.join(n + u for n, u in parts) != value:
        raise ValueError('Unsupported GC TTL')
    return sum(float(n) * {'s': 1, 'm': 60, 'h': 3600}[u] for n, u in parts)

def eligible(card, number, now, ttl, project_id):
    if card.get('project_id') != project_id or card.get('number') != number:
        return False
    if card.get('status') not in ('done', 'cancelled'):
        return False
    updated = datetime.datetime.fromisoformat(card['updated_at'].replace('Z', '+00:00')).timestamp()
    return now - updated > ttl

def command(args):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=120).stdout.strip()

def git(path, *args):
    return command(['git', '-C', str(path), *args])

def active(path):
    prefix = str(path) + '/'
    for entry in Path('/proc').iterdir():
        if not entry.name.isdigit() or int(entry.name) == os.getpid():
            continue
        try:
            cwd = os.readlink(entry / 'cwd')
            if cwd == str(path) or cwd.startswith(prefix):
                return True
            if entry.stat().st_uid == os.getuid():
                args = (entry / 'cmdline').read_bytes().split(b'\0')
                if any(a.decode(errors='replace') == str(path) or a.decode(errors='replace').startswith(prefix) for a in args):
                    return True
        except (FileNotFoundError, ProcessLookupError):
            continue
        except PermissionError:
            # Other users' processes are not Multica agent processes.
            continue
    return False

def sweep(apply, settings):
    cfg = json.loads(settings.daemon_config.read_text())
    ttl = duration(cfg['gc_ttl'])
    if ttl <= 0:
        raise ValueError('GC workspace TTL must be positive')
    results = []
    candidates = [(root, allow, path) for root, allow in settings.roots
                  if root.is_dir() and not root.is_symlink() for path in sorted(root.iterdir())]
    for root, allow, path in candidates:
        number = candidate_number(path.name, allow_number_only=allow, prefix=settings.prefix)
        if number is None or path.is_symlink() or not path.is_dir():
            continue
        result = {'path': str(path), 'card': f'{settings.prefix}-{number}'}
        try:
            card = json.loads(command(['multica', 'issue', 'get', f'{settings.prefix}-{number}', '--output', 'json']))
            if not eligible(card, number, time.time(), ttl, settings.project_id):
                result['result'] = 'kept: card not terminal past workspace TTL'
            elif active(path):
                result['result'] = 'kept: active process'
            elif not (path / '.git').is_file():
                result['result'] = 'kept: not a registered Git worktree'
            elif git(path, 'status', '--porcelain', '--untracked-files=all'):
                result['result'] = 'kept: local changes'
            else:
                worktrees = git(path, 'worktree', 'list', '--porcelain')
                if 'worktree ' + str(path) not in worktrees.splitlines():
                    raise ValueError('Git registration does not match directory')
                # Refresh the existing checkout's remote reference; never change its branch.
                git(path, 'fetch', '--quiet', settings.remote, settings.branch)
                if any(line.startswith('+') for line in git(path, 'cherry', settings.remote + '/' + settings.branch, 'HEAD').splitlines()):
                    result['result'] = 'kept: commits not represented on configured base'
                elif not apply:
                    result['result'] = 'would remove'
                else:
                    card = json.loads(command(['multica', 'issue', 'get', f'{settings.prefix}-{number}', '--output', 'json']))
                    if not eligible(card, number, time.time(), ttl, settings.project_id) or active(path):
                        raise ValueError('Card or process state changed')
                    if git(path, 'status', '--porcelain', '--untracked-files=all'):
                        raise ValueError('Local changes appeared')
                    git(path, 'worktree', 'remove', str(path))
                    result['result'] = 'removed' if not path.exists() else 'error: directory remains'
        except Exception as exc:
            result['result'] = 'kept: check failed (' + type(exc).__name__ + ')'
        results.append(result)
        print(json.dumps(result), flush=True)
    return results

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--config', required=True, help='Standalone sweep configuration JSON')
    args = parser.parse_args()
    settings = load_settings(args.config)
    with open(settings.lock_file, 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        sweep(args.apply, settings)
