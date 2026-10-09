#!/usr/bin/env python3
"""Check standalone and embedded JavaScript syntax without a browser."""
import re
import subprocess
import tempfile
from pathlib import Path
root=Path(__file__).resolve().parents[1]
for name in ('graph.js','desk.js','quality.js','library.js','sw.js'):
    subprocess.run(['node','--check',str(root/name)],check=True)
with tempfile.TemporaryDirectory() as directory:
    path=Path(directory)/'inline.js'
    path.write_text('\n'.join(re.findall(r'<script>(.*?)</script>',(root/'index.html').read_text(),re.S)))
    subprocess.run(['node','--check',str(path)],check=True)
print('Standalone and embedded JavaScript syntax checks pass.')
