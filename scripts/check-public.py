#!/usr/bin/env python3
"""Check the release boundary before publishing. Do not print matched content."""
import json
import re
from pathlib import Path

root = Path(__file__).resolve().parents[1]
failures = []
# Build the private name fragments separately so the checker does not match itself.
patterns = [re.compile(p, re.I) for p in (
    r'registry[.]lab[.]', r'(?:candle' + r'keep|graven' + r'hollow|str' + r'ahd)[.]',
    r'/Users/' + r'bi' + r'lly/', r'Mr[.]?\s+' + r'Bea' + r'ker',
    r'192[.]168[.]100[.]', r'gh[pousr]_[A-Za-z0-9]{30,}',
    r'github_pat_[A-Za-z0-9_]{30,}', r'AKIA[A-Z0-9]{16}',
    r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----')]
for file in root.rglob('*'):
    if not file.is_file() or '.git' in file.parts or '__pycache__' in file.parts:
        continue
    path = file.relative_to(root)
    if file.suffix in {'.pdf', '.epub', '.parquet', '.sqlite', '.db', '.tgz', '.png', '.jpg'} or 'evidence' in path.parts or 'private-config' in path.parts:
        failures.append(str(path) + ': private data or binary artifact')
        continue
    if path.name == 'research-assistant':
        failures.append(str(path) + ': compiled binary')
        continue
    try:
        data = file.read_text()
    except UnicodeDecodeError:
        failures.append(str(path) + ': unexpected binary')
        continue
    for number, line in enumerate(data.splitlines(), 1):
        if any(pattern.search(line) for pattern in patterns):
            failures.append(f'{path}:{number}: private identifier or credential pattern')
for name in ('books.json', 'quality_generation_audit.json'):
    if json.loads((root / name).read_text()) != {}:
        failures.append(name + ': private manifest must be empty')
if failures:
    raise SystemExit('\n'.join(failures))
print('Public release boundary passes: no private deployment material or credential patterns found.')
