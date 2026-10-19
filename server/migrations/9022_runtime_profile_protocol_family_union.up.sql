-- Reconcile the protocol-family whitelist after the upstream sync.
-- Upstream 583 rebuilt runtime_profile_protocol_family_check to add `muse`
-- without `zcode`; this fork's 9011 rebuilt it to add `zcode` without `muse`.
-- Whichever ran last won, so installs that applied 9011 before the sync
-- reject muse and fresh installs reject zcode after 583 until 9011 runs.
-- Rebuilding the constraint as the union heals every prior state; the
-- NOT VALID + DROP IF EXISTS pattern matches 583/9011.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'codearts',
        'openclaw',
        'hermes',
        'pi',
        'cursor',
        'kimi',
        'reasonix',
        'dsh',
        'kiro',
        'antigravity',
        'qoder',
        'qoderclicn',
        'traecli',
        'deveco',
        'grok',
        'qwen',
        'qwenpaw',
        'mcode',
        'muse',
        'dim',
        'zeroclaw',
        'zcode'
    )) NOT VALID;
