export const meta = {
  name: 'mandat-slice-close',
  description: 'Verify a landed code slice, reconcile governed-doc drift, and red-team each flip candidate into one owner-decision packet',
  phases: [
    { title: 'Verify', detail: 'gate-verify the diff + doc-keeper reconcile drift' },
    { title: 'RedTeam', detail: 'one red-team per flip-candidate governed doc' },
  ],
}

// args: {
//   changeSummary: string   // one paragraph: what the landed code actually does (feeds the red-teamers)
//   flips: [{ us, target, doc }]  // governed docs whose status the owner intends to advance
// }
const changeSummary = (args && args.changeSummary) ||
  'A code slice has landed on main. Read the recent commits and the cited docs to learn what it does.'
const flips = (args && args.flips) || []

const GATE = {
  type: 'object', additionalProperties: false,
  required: ['verdict', 'gates', 'findings'],
  properties: {
    verdict: { type: 'string', enum: ['SAFE-TO-COMMIT', 'BLOCK'] },
    gates: { type: 'string' },
    findings: { type: 'array', items: { type: 'string' } },
  },
}
const RECONCILE = {
  type: 'object', additionalProperties: false,
  required: ['edits'],
  properties: {
    edits: {
      type: 'array',
      items: {
        type: 'object', additionalProperties: false,
        required: ['doc', 'reason', 'proposed'],
        properties: { doc: { type: 'string' }, reason: { type: 'string' }, proposed: { type: 'string' } },
      },
    },
  },
}
const REDTEAM = {
  type: 'object', additionalProperties: false,
  required: ['us', 'verdict', 'acSummary', 'sourcesExist', 'killCriterion'],
  properties: {
    us: { type: 'string' },
    verdict: { type: 'string', enum: ['flip-as-is', 'flip-after-reconcile', 'blocked'] },
    acSummary: { type: 'string' },
    reconciledText: { type: 'string', description: 'exact doc edits needed before the flip is honest; empty if flip-as-is' },
    sourcesExist: { type: 'boolean' },
    killCriterion: { type: 'string' },
  },
}

phase('Verify')
const [gate, reconcile] = await parallel([
  () => agent(
    `Independently verify the current repo state is commit-ready. Re-run \`make check\` from scratch (report the real exit code) and \`npx govkit check\`. Check scope, and do an independent simplify/quality pass (duplication, dead code, comment policy, cited-doc-ids-exist). Trust no prior summary. Context on what landed: ${changeSummary}`,
    { agentType: 'gate-verifier', model: 'opus', phase: 'Verify', label: 'gate-verify', schema: GATE },
  ),
  () => agent(
    `Reconcile governed-doc drift against what the code actually does now. Context: ${changeSummary}. For each governed doc that names a symbol, mechanism, or gap the code has since changed, propose the EXACT replacement text; do NOT apply, do NOT flip any status. Docs to check: ${flips.map(f => f.doc).join(', ')}.`,
    { agentType: 'swe-flow:doc-keeper', model: 'sonnet', phase: 'Verify', label: 'reconcile drift', schema: RECONCILE },
  ),
])

phase('RedTeam')
const redTeam = (await parallel(flips.map(f => () =>
  agent(
    `Run the spec-red-team pass on ${f.doc} BEFORE its owner flips it to "${f.target}". Context on what landed: ${changeSummary}. Assess which ACs are met/partial/not-yet with file:line; whether "${f.target}" is honest; verify every cited Source exists; steelman, falsifiable Fails-if, self-refute, one kill criterion. If the doc must be reworded before the flip is honest, give the EXACT reconciled text. You flip nothing.`,
    { agentType: 'red-teamer', model: 'opus', phase: 'RedTeam', label: `red-team ${f.us}`, schema: REDTEAM },
  ).then(b => ({ ...b, us: f.us, target: f.target, doc: f.doc }))
)))

return { gate, reconcile, redTeam, humanGates: flips.map(f => `${f.us} -> ${f.target}`) }
