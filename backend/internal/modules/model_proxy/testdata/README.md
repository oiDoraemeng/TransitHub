These fixtures retain the field and event structure of previously recorded
enterprise Claude responses. They contain no original signatures, reasoning,
account identifiers, or message text.

- `enterprise_thinking_format.json`: derived from the saved
  `comprehensive-20261128-104958/thinking_first.raw.json` response body.
- `enterprise_thinking_format.sse`: derived from saved capture
  `full-record-20261128-0330/captures/002-ccrec-20261128-0330-2-adb0b9e76cd8/response.sse`.

Signatures and thinking text are empty; identifiers and answer text are
placeholders. These fixtures describe a wire format, not a valid signed turn.
Tests compare the mapped thinking fields/events against this saved structure.
