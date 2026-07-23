# Agent Detail Empty State Design

## Goal

Make the Agent detail panel explain why no detail is shown, without changing
scheduler APIs, persistence, or adding a call to action.

## State rules

The detail panel selects its copy from the currently selected Agent and the
active and archived Agent collections:

1. When no active Agent exists and none is selected, render `当前没有运行中的
   Agent` and `启动 Agent 后，可在这里查看运行状态和配置信息`. Do not render
   `选择一个 agent 查看详情`.
2. When active Agents exist but no Agent is selected, retain `选择一个 agent
   查看详情`.
3. When a participant from context history is neither active nor available in
   archived metadata, render `该 Agent 已离线，且没有可用的归档详情`. Its row
   may remain selectable so the participant is identifiable, but selection
   must produce this visible response.
4. When archived metadata is available, retain the existing read-only detail
   behavior, including `已归档 · 只读`.

## Implementation

Keep the logic in the client-side `renderDetail` path and its unavailable
detail helper. Mirror the asset change to the plugin source bundle, following
the repository's asset parity contract. No backend behavior changes.

## Verification

Playwright coverage will exercise the empty scheduler, an active-but-unselected
client state, an unavailable historical participant, and the existing archived
participant flow. The full UI suite and Go test suite remain required.
