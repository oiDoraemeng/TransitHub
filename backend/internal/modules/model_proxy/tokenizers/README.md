# Bundled tokenizer

`gemma3.model.gz` is a gzip-compressed copy of the model referenced by Google's
experimental text-only LocalTokenizer. No model weights or inference runtime
are included. The pinned source, SHA-256, and covered model IDs are recorded in
`../token_range_profiles.json` and `../token_range.go`.

Source: https://github.com/google/gemma_pytorch/blob/014acb7ac4563a5f77c76d7ff98f31b568c16508/tokenizer/gemma3_cleaned_262144_v2.spiece.model

Registry: https://github.com/googleapis/python-genai/blob/main/google/genai/_local_tokenizer_loader.py

Rebuild with `scripts/token-range/calibrate.py` from the repository root. Both
this model and OpenAI vocabularies are bundled at build time: public requests
must never download tokenizer assets.
