#!/usr/bin/env python3
"""Build a Mattermost bundle without macOS AppleDouble/xattr entries."""
import json
import pathlib
import tarfile
root = pathlib.Path(__file__).resolve().parent.parent
manifest = json.loads((root / 'plugin.json').read_text())
plugin_id, version = manifest['id'], manifest['version']
output = root / 'dist' / f'{plugin_id}-{version}.tar.gz'
output.parent.mkdir(exist_ok=True)
paths = ['plugin.json', 'server/dist/plugin-linux-amd64', 'server/dist/plugin-linux-arm64', 'webapp/dist/main.js']
with tarfile.open(output, 'w:gz', format=tarfile.USTAR_FORMAT) as archive:
    for path in paths:
        archive.add(root / path, arcname=f'{plugin_id}/{path}', recursive=False)
with tarfile.open(output) as archive:
    assert len(archive.getmembers()) == len(paths)
    assert all('/._' not in m.name for m in archive.getmembers())
print(output)
