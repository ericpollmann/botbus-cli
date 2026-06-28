#!/usr/bin/env node
// Reads e2e/friction_log.jsonl and prints a trend report.
// Usage: node e2e/analyze_friction.js
//
// Shows: score trend, most common friction points, top suggestions.
// Run after a batch of /loop iterations to decide what to fix next.

const fs = require('fs')
const path = require('path')

const LOG = path.join(__dirname, 'friction_log.jsonl')

if (!fs.existsSync(LOG)) {
  console.error('No friction_log.jsonl found. Run e2e/haiku_e2e.js first.')
  process.exit(1)
}

const runs = fs.readFileSync(LOG, 'utf8')
  .split('\n')
  .filter(Boolean)
  .map((line, i) => {
    try { return JSON.parse(line) }
    catch (e) { console.warn(`Line ${i+1} malformed, skipping`); return null }
  })
  .filter(Boolean)

console.log(`\n=== botbus E2E Friction Report — ${runs.length} run(s) ===\n`)

// Score trend
const scores = runs.map(r => r.judge?.score ?? 0)
console.log('Score trend (newest last):')
console.log('  ' + scores.map(s => `${s}`.padStart(3)).join(' → '))
if (scores.length > 1) {
  const delta = scores[scores.length-1] - scores[0]
  console.log(`  Δ from first to last: ${delta >= 0 ? '+' : ''}${delta}`)
}

// Success rate
const successes = runs.filter(r => r.judge?.success).length
console.log(`\nSuccess rate: ${successes}/${runs.length} (${Math.round(100*successes/runs.length)}%)`)

// File production rates
const feOk = runs.filter(r => r.judge?.fe_file_ok).length
const beOk = runs.filter(r => r.judge?.be_file_ok).length
const coordOk = runs.filter(r => r.judge?.coordination_ok).length
console.log(`FE file ok:        ${feOk}/${runs.length}`)
console.log(`BE file ok:        ${beOk}/${runs.length}`)
console.log(`Coordination ok:   ${coordOk}/${runs.length}`)

// Agent-level stats
const feSubscribed = runs.filter(r => r.fe?.subscribed).length
const feTask = runs.filter(r => r.fe?.task_received).length
const feFile = runs.filter(r => r.fe?.file_written).length
const beSubscribed = runs.filter(r => r.be?.subscribed).length
const beTask = runs.filter(r => r.be?.task_received).length
const beFile = runs.filter(r => r.be?.file_written).length
console.log('\nAgent self-reports:')
console.log(`  FE: subscribed ${feSubscribed}/${runs.length}  task ${feTask}/${runs.length}  file ${feFile}/${runs.length}`)
console.log(`  BE: subscribed ${beSubscribed}/${runs.length}  task ${beTask}/${runs.length}  file ${beFile}/${runs.length}`)

// Friction point frequency
const counts = {}
runs.forEach(r => {
  ;(r.judge?.friction_points ?? []).forEach(fp => {
    const key = `[${fp.severity}] ${fp.agent}/${fp.phase}: ${fp.issue}`
    counts[key] = (counts[key] ?? 0) + 1
  })
  ;(r.fe?.friction ?? []).forEach(f => {
    counts[`[fe-self] ${f}`] = (counts[`[fe-self] ${f}`] ?? 0) + 1
  })
  ;(r.be?.friction ?? []).forEach(f => {
    counts[`[be-self] ${f}`] = (counts[`[be-self] ${f}`] ?? 0) + 1
  })
})

const sorted = Object.entries(counts).sort((a,b) => b[1]-a[1])
console.log('\nTop friction points (most frequent first):')
sorted.slice(0, 15).forEach(([issue, n]) => {
  console.log(`  x${n}  ${issue}`)
})

// Suggestions (deduplicated)
const allSuggestions = {}
runs.forEach(r => {
  ;(r.judge?.suggestions ?? []).forEach(s => {
    allSuggestions[s] = (allSuggestions[s] ?? 0) + 1
  })
})
const topSuggestions = Object.entries(allSuggestions).sort((a,b) => b[1]-a[1])
console.log('\nTop improvement suggestions:')
topSuggestions.slice(0, 8).forEach(([s, n]) => {
  console.log(`  x${n}  ${s}`)
})

// Channel IDs for reference
console.log('\nChannel IDs (newest last):')
runs.slice(-5).forEach(r => console.log(`  ${r.channel_id}`))

console.log()
