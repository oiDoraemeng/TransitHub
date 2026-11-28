"""Import pinned public Gemini samples without synthesizing or mixing outputs.

Usage: python scripts/prepare-gemini-fingerprint-assets.py unified_reference.jsonl
The source file must match the pinned revision and digest below.
"""
import argparse
import collections
import gzip
import hashlib
import json
import math
from pathlib import Path
import re
import unicodedata

REVISION = '643587185bf9c54b0c94ec6b2bed600f764177ec'
SOURCE_SHA256 = 'eadd1d25f5232b6e4b65152b5d953d2656e4372327c2c41754486ca80f880765'
SOURCE_PATH = 'data/unified_reference.jsonl'
SOURCE_URL = f'https://raw.githubusercontent.com/Ikaleio/lm-detector/{REVISION}/{SOURCE_PATH}'


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def integers(text):
    # Same longest uninterrupted integer-run convention as the existing import.
    runs, current, end = [], [], 0
    for match in re.finditer(r'[0-9]+', text):
        if current and any(unicodedata.category(c).startswith('L') for c in text[end:match.start()]):
            runs.append(current)
            current = []
        value = int(match[0])
        if 1 <= value <= 355:
            current.append(value)
        end = match.end()
    runs.append(current)
    return max(runs, key=len)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=Path)
    args = parser.parse_args()
    raw = args.source.read_bytes()
    if digest(raw) != SOURCE_SHA256:
        raise ValueError('Source digest differs from the pinned public reference')
    assets = Path(__file__).resolve().parents[1] / 'backend/internal/modules/model_proxy/fingerprints'
    bank_path = assets / 'reference.json.gz'
    bank = json.loads(gzip.decompress(bank_path.read_bytes()))
    retained = [s for s in bank['samples'] if not s['model'].startswith('gemini-')]
    pool, provenance, rejected = {}, [], []
    candidates = collections.Counter()
    for line_number, line in enumerate(raw.decode('utf-8-sig').splitlines(), 1):
        row = json.loads(line)
        model = row['model']['id']
        if not model.startswith('gemini-'):
            continue
        requested = row.get('request', {}).get('model', model).removeprefix('google/')
        if requested != model:
            raise ValueError(f'Ambiguous model label on source line {line_number}')
        for sample in row['samples']:
            candidates[model] += 1
            seq = integers(sample['text'])
            response = (sample.get('response_model') or '').removeprefix('google/')
            reason = None
            if sample.get('error'):
                reason = 'source_error'
            elif sample.get('completion') in ('truncated', 'incomplete', 'error', 'failed'):
                reason = 'incomplete'
            elif response and response != model:
                reason = 'response_model_mismatch'
            elif len(seq) < max(189, math.ceil((sample.get('expected_count') or 0) * .55)):
                reason = 'too_short'
            if reason:
                rejected.append(dict(model=model, sample_id=sample['id'], reason=reason))
                continue
            sample_id = digest(json.dumps(seq, separators=(',', ':')).encode())
            key = (model, sample_id)
            if key in pool:
                raise ValueError(f'Duplicate reference sequence for {model}')
            pool[key] = dict(model=model, text=sample['text'], numbers=seq, id=sample_id)
            provenance.append(dict(model=model, sample_id=sample['id'], sequence_sha256=sample_id,
                                   text_sha256=digest(sample['text'].encode()), source_line=line_number,
                                   request_model=requested, response_model=sample.get('response_model'),
                                   completion=sample.get('completion')))
    counts = collections.Counter(s['model'] for s in pool.values())
    if len(counts) != 11 or min(counts.values(), default=0) <= 30:
        raise ValueError(f'Insufficient Gemini model pools: {dict(counts)}')
    bank['samples'] = retained + [pool[k] for k in sorted(pool)]
    encoded = json.dumps(bank, ensure_ascii=False, separators=(',', ':')).encode()
    if len(encoded) > 16 << 20:
        raise ValueError('Reference bank exceeds the runtime decoding limit')
    with bank_path.open('wb') as output:
        with gzip.GzipFile(fileobj=output, mode='wb', mtime=0) as archive:
            archive.write(encoded)
    manifest = dict(source=dict(repository='Ikaleio/lm-detector', revision=REVISION,
                                path=SOURCE_PATH, url=SOURCE_URL, sha256=SOURCE_SHA256, bytes=len(raw)),
                    samples=len(pool), models=dict(sorted(counts.items())), candidates=dict(sorted(candidates.items())),
                    rejected=rejected, records=provenance,
                    notes=['Original source labels and output text retained; model identity has not been independently authenticated.',
                           'No synthetic numbers, padding, truncation, shuffling or cross-model sample substitution.',
                           'Samples with legacy unknown completion status are retained only when their original integer run meets the import minimum.'])
    (assets / 'gemini-source-manifest.json').write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(dict(models=dict(sorted(counts.items())), samples=len(pool), rejected=len(rejected),
                          total_models=len({s['model'] for s in bank['samples']}), total_samples=len(bank['samples'])), indent=2))


if __name__ == '__main__':
    main()
