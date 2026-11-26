"""Rebuild empirical byte-feature ranges using independent local tokenizers.

Development only: pip install tiktoken==0.12.0 sentencepiece requests
No inference requests, API credentials, customer prompts, or upstream usage.
"""
import hashlib
import json
import math
import random
from pathlib import Path

import gzip
import statistics
import requests
import sentencepiece as spm
import tiktoken

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
OUT = ROOT / 'backend/internal/modules/model_proxy'
SAMPLE = 16384
WINDOWS = 64
GEMMA_URL = 'https://raw.githubusercontent.com/google/gemma_pytorch/014acb7ac4563a5f77c76d7ff98f31b568c16508/tokenizer/gemma3_cleaned_262144_v2.spiece.model'
GEMMA_HASH = '1299c11d7cf632ef3b4e11937501358ada021bbdf7c47638d13c0ee982f2e79c'


def sample_parts(text):
    raw = text.encode()
    if len(raw) <= SAMPLE:
        return [text], True
    width = SAMPLE // WINDOWS
    samples = []
    for i in range(WINDOWS):
        offset = (len(raw)-width)*i//(WINDOWS-1)
        lo, hi = max(0, offset-32), min(len(raw), offset+width+32)
        samples.append(raw[lo:hi].decode('utf-8', errors='ignore'))
    return samples, False


def estimate(encode, text):
    samples, exact = sample_parts(text)
    counts = [len(encode(sample)) for sample in samples]
    if exact:
        return counts[0], counts[0]
    densities = [count/len(sample.encode()) for count, sample in zip(counts, samples)]
    mean = sum(counts)/sum(len(sample.encode()) for sample in samples)
    error = 3*statistics.stdev(densities)/math.sqrt(len(densities))
    n = len(text.encode())
    return max(0, mean-error)*n, (mean+error)*n


def corpus():
    rng = random.Random(4276040)
    paragraphs = [
        'The request includes the complete conversation history, system instructions and tool results. Please review the code and explain the result.\n',
        '请检查完整历史消息、系统提示词、工具定义和工具返回内容。缓存读取和写入不能重复计算。这是独立生成的测试语料。\n',
        '日本語の文章で入力トークンの計算方法を検証します。履歴とツールの結果を含めます。\n',
        'هذه فقرة لاختبار حساب الرموز مع سجل المحادثة الكامل ونتائج الأدوات.\n',
        'यह बातचीत के इतिहास और उपकरणों के परिणामों सहित टोकन गणना का परीक्षण है।\n',
        'Проверяем точность подсчёта токенов с полной историей сообщений и результатами инструментов.\n',
        '입력 토큰의 계산 방법을 확인하고 대화 기록과 도구 실행 결과를 포함합니다.\n',
        'Status ✅ processing 🚀 结果正常 🙂🌍 Unicode ∑∇∫ αβγ\n',
        '0123456789'*25, 'a'*251, '中'*123, ' '*256, '\n'*256,
        ',.!?{}[]():;+-=*/_\\"\''*25,
        ''.join(chr(rng.randrange(0x4e00, 0x9fff)) for _ in range(1000)),
        ''.join(chr(rng.randrange(0x20000, 0x2a6df)) for _ in range(1000)),
        ''.join(rng.choice('ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=') for _ in range(4096)),
    ]
    for path in sorted((ROOT/'backend/internal/modules/model_proxy').glob('*.go'))[:45]:
        paragraphs.append(path.read_text(encoding='utf-8'))
    for path in [ROOT/'README_CN.md', ROOT/'README.md',
                 ROOT/'frontend/src/locales/zh-CN.ts', ROOT/'frontend/src/locales/en-US.ts']:
        paragraphs.append(path.read_text(encoding='utf-8'))
    rows = []
    # Independent offsets, lengths, and heterogeneous mixtures. Split before fit.
    for index, text in enumerate(paragraphs):
        for size in (37, 173, 1021, 7907, 16387, 65537):
            offset = rng.randrange(max(1, len(text)))
            source = text[offset:] + text[:offset]
            value = (source * (size//len(source) + 1))[:size]
            rows.append((f'pattern-{index}-{size}', value))
    for i in range(180):
        chunks = []
        for _ in range(rng.randrange(2, 20)):
            source = rng.choice(paragraphs)
            size = rng.randrange(31, 12000)
            chunks.append((source * (size//len(source) + 1))[:size])
        rows.append((f'mixed-{i}', ''.join(chunks)))
    rng.shuffle(rows)
    return rows


def main():
    cache = HERE / '.cache'
    cache.mkdir(exist_ok=True)
    model = cache/'gemma3.model'
    if not model.exists():
        response = requests.get(GEMMA_URL, timeout=120)
        response.raise_for_status()
        model.write_bytes(response.content)
    assert hashlib.sha256(model.read_bytes()).hexdigest() == GEMMA_HASH
    gemma = spm.SentencePieceProcessor(model_file=str(model))
    tokenizers = {name: tiktoken.get_encoding(name).encode_ordinary
                  for name in ('o200k_base', 'cl100k_base')}
    tokenizers['gemma3'] = gemma.encode_as_ids
    rows = corpus()
    calibration_end = len(rows)*4//5
    profiles = {}
    validation = []
    for name, encode in tokenizers.items():
        corpus_hash = hashlib.sha256(''.join(text for _, text in rows).encode()).hexdigest()
        counts_cache = cache/f'{name}-{corpus_hash}.json'
        if counts_cache.exists():
            ys = json.loads(counts_cache.read_text())
        else:
            ys = [len(encode(text)) for _, text in rows]
            counts_cache.write_text(json.dumps([int(x) for x in ys]))
        bounds_cache = cache/f'{name}-sample-{corpus_hash}-{SAMPLE}-{WINDOWS}.json'
        if bounds_cache.exists():
            estimates = json.loads(bounds_cache.read_text())
        else:
            estimates = [estimate(encode, text) for _, text in rows]
            bounds_cache.write_text(json.dumps(estimates))
        lower_factor = min([1.0] + [ys[i]/lo for i,(lo,hi) in enumerate(estimates[:calibration_end]) if lo>0])
        upper_factor = max([1.0] + [ys[i]/max(1,hi) for i,(lo,hi) in enumerate(estimates[:calibration_end])])
        profiles[name] = {'lower_factor':lower_factor, 'upper_factor':upper_factor}
        covered = 0
        for i in range(calibration_end, len(rows)):
            lo, hi = estimates[i]
            lower = math.floor(lo*lower_factor)
            upper = math.ceil(hi*upper_factor)
            covered += lower <= ys[i] <= upper
            validation.append({'profile': name, 'name': rows[i][0], 'bytes': len(rows[i][1].encode()),
                               'actual': int(ys[i]), 'min': lower, 'max': upper,
                               'covered': bool(lower <= ys[i] <= upper),
                               'cap_40_covers': bool(ys[i] <= math.ceil(upper*1.4))})
        print(f'{name}: held-out coverage {covered}/{len(rows)-calibration_end}', flush=True)
    payload = {'version': 'sampled-tokenizer-range-v1', 'sample_bytes': SAMPLE, 'sample_windows': WINDOWS,
               'kind': 'empirical_calibration_range_not_guaranteed_bounds',
               'sources': {'openai': 'https://github.com/openai/tiktoken', 'gemini': GEMMA_URL,
                           'gemini_sha256': GEMMA_HASH},
               'profiles': profiles}
    (OUT/'tokenizers').mkdir(exist_ok=True)
    (OUT/'tokenizers/gemma3.model.gz').write_bytes(gzip.compress(model.read_bytes(), mtime=0))
    (OUT/'token_range_profiles.json').write_text(json.dumps(payload, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
    summary = {'calibration': calibration_end, 'held_out': len(rows)-calibration_end,
               'validation': validation, 'claude': 'No current official local tokenizer or count API samples: use conservative UTF-8 byte fallback, not fitted GPT coefficients.'}
    (HERE/'validation.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
    fixtures = []
    for text in ['你好，世界！这是一个中文输入测试。'*20,
                 'A complete request contains system prompts, history and tools. '*20,
                 '0123456789'*100, '中'*1000, '𠀀'*1000,
                 'Status 🚀🙂🌍 正常 '*80, ' '*1000]:
        fixtures.append({'text': text, 'counts': {name: len(encode(text)) for name, encode in tokenizers.items()}})
    for _, text in rows[:40]:
        sample = sample_parts(text)[0][0]
        fixtures.append({'text':sample, 'counts':{name:len(encode(sample)) for name,encode in tokenizers.items()}})
    (OUT/'testdata').mkdir(exist_ok=True)
    (OUT/'testdata/token_range_reference.json').write_text(json.dumps(fixtures, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')


if __name__ == '__main__':
    main()
