"""Import previously verified public reference records; never synthesize sequences."""
import gzip
import hashlib
import json
import pathlib
import shutil
import sys

source = pathlib.Path(sys.argv[1])
target = pathlib.Path(__file__).resolve().parents[1] / 'backend/internal/modules/model_proxy'
assets = target / 'fingerprints'
assets.mkdir(exist_ok=True)
profiles = json.loads((source / 'modeltrace-compatibility/combined-profiles.json').read_text(encoding='utf-8'))
suite = json.loads((source / 'modeltrace-compatibility/python-fixtures.json').read_text(encoding='utf-8'))['enrollment_suite']
records = []
seen = set()
for line in (source / 'expanded-reference-pool.jsonl').read_text(encoding='utf-8').splitlines():
    r = json.loads(line)
    assert hashlib.sha256(r['text'].encode()).hexdigest() == r['text_sha256']
    assert hashlib.sha256(json.dumps(r['numbers'], separators=(',', ':')).encode()).hexdigest() == r['sequence_sha256']
    assert len(r['numbers']) == r['valid_count'] >= 189
    assert all(isinstance(n, int) and 1 <= n <= 355 for n in r['numbers'])
    assert (r['model'], r['sequence_sha256']) not in seen
    seen.add((r['model'], r['sequence_sha256']))
    records.append(dict(model=r['model'], text=r['text'], numbers=r['numbers'], id=r['sequence_sha256']))
data = dict(profiles=profiles, fixed=[dict(text=r['final_user_text'], count=r['expected_count'], format=r['format']) for r in suite], samples=records)
with (assets / 'reference.json.gz').open('wb') as f:
    with gzip.GzipFile(fileobj=f, mode='wb', mtime=0) as z:
        z.write(json.dumps(data, ensure_ascii=False, separators=(',', ':')).encode())
for name in ['LICENSE-lm-detector.txt', 'LICENSE-ModelTrace.txt']:
    shutil.copyfile(source / name, assets / name)
print(json.dumps(dict(samples=len(records), models=len({r['model'] for r in records}), profiles=len(profiles), fixed=len(suite))))
