ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

-- Widen the whitelist to include Cline 3.x NDJSON (`cline`). The family backs
-- both PATH `cline` / MULTICA_CLINE_PATH and Custom Runtime Profiles whose
-- command_name points at an internal Cline-compatible binary. NOT VALID mirrors
-- migrations 126/134/136/175/179/202 so a historical Gemini row they intentionally
-- tolerated does not block the upgrade. Includes `deveco` (175), `grok` (179),
-- and `qwen` (202).
--
-- Numbered 9001 (fork-high range) so long-lived fork merges with upstream do
-- not keep colliding with sequential upstream prefixes; renumber to the next
-- sequential id only if/when this lands on multica-ai/multica.
ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'openclaw',
        'hermes',
        'pi',
        'cursor',
        'kimi',
        'kiro',
        'antigravity',
        'qoder',
        'traecli',
        'deveco',
        'grok',
        'qwen',
        'cline'
    )) NOT VALID;
