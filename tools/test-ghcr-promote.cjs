const assert = require('node:assert/strict');
const vm = require('node:vm');
// Run command is documented in docs/ghcr-publication.md. Reads the actual parsed YAML.
const workflow = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const {gate, merge, promote, 'pull-check': pullCheck} = workflow.jobs;
const sha = 'a'.repeat(40), digest = `sha256:${'b'.repeat(64)}`;
const image = 'ghcr.io/leriusss/tg-spam';
assert.deepEqual([...promote.needs].sort(), ['gate', 'merge', 'pull-check']);
assert.deepEqual([...pullCheck.needs].sort(), ['gate', 'merge']);
assert.equal(workflow.concurrency.group, 'ghcr-publication');
assert.equal(workflow.concurrency['cancel-in-progress'], false);
assert.equal(promote.concurrency, undefined); // inherits the workflow-wide lock
assert.equal(merge.outputs.digest, '${{ steps.manifest.outputs.digest }}');
assert.equal(pullCheck.env.DIGEST, '${{ needs.merge.outputs.digest }}');
const move = promote.steps.find(s => s.with?.script);
assert.equal(move.env.DIGEST, '${{ needs.merge.outputs.digest }}');
assert.equal(promote.env.SHA, '${{ needs.gate.outputs.sha }}');
assert.equal(promote.env.IMAGE, '${{ needs.gate.outputs.image }}');
assert(!promote.steps.some(s => /checkout|build-push-action/.test(s.uses || '')));
// master can be mutated only in promote; no workflow step may write latest.
for (const [job, spec] of Object.entries(workflow.jobs)) {
  for (const step of spec.steps) {
    const code = `${step.run || ''}\n${step.with?.script || ''}`;
    if (code.includes(':master')) assert.equal(job, 'promote');
    assert(!code.includes(':latest'));
  }
}
console.log('PASS job dependencies, shared digest, no rebuild, serialization, tag write scope');
function allowed(automatic, results = {}) {
  const needs = Object.fromEntries(['gate','merge','pull-check'].map(name => [name, {
    result: results[name] || 'success', outputs: name === 'gate' ? {automatic} : {}
  }]));
  return vm.runInNewContext(promote.if, {
    needs, success: () => Object.values(needs).every(n => n.result === 'success')
  });
}
assert.equal(allowed('true'), true);
console.log('PASS successful pull-check permits automatic promotion');
for (const result of ['failure', 'cancelled', 'skipped']) {
  assert.equal(allowed('true', {'pull-check': result}), false);
  console.log(`PASS ${result} pull-check blocks promotion`);
}
for (const dep of ['gate','merge']) assert.equal(allowed('true', {[dep]:'failure'}), false);
assert.equal(allowed('false'), false);
console.log('PASS failed dependencies and manual verification block promotion');
const AsyncFunction = Object.getPrototypeOf(async function(){}).constructor;
const runPromotion = new AsyncFunction('context','github','core','exec','process', move.with.script);
const runGate = new AsyncFunction('context','github','core', gate.steps[0].with.script);
(async () => {
  const out = {};
  await runGate({repo:{owner:'leriusss',repo:'tg-spam'},sha,eventName:'workflow_dispatch',
    payload:{inputs:{mode:'verify-publish'}}},
    {paginate: async()=>[{head_sha:sha,head_repository:{full_name:'leriusss/tg-spam'},
      path:'.github/workflows/ci.yml',event:'push'}],rest:{actions:{listWorkflowRuns(){}}}},
    {setOutput:(k,v)=>out[k]=v});
  assert.equal(out.publish, 'true');
  assert.equal(out.automatic, 'false');
  const manifest = merge.steps.find(s => s.id === 'manifest');
  assert.equal(manifest.env.AUTOMATIC, '${{ needs.gate.outputs.automatic }}');
  // Execute the actual tag selection shell fragment without Docker or registry calls.
  const selection = manifest.run.slice(manifest.run.indexOf('tag="verify-'), manifest.run.indexOf('docker buildx imagetools create'));
  const {execFileSync} = require('node:child_process');
  // Use platform Git Bash on Windows; Linux/macOS can resolve bash directly.
  const bash = process.platform === 'win32' ? 'C:/Program Files/Git/bin/bash.exe' : 'bash';
  const tag = execFileSync(bash, ['-c', selection + '\nprintf "%s" "$tag"'], {
    encoding:'utf8', env:{...process.env,AUTOMATIC:out.automatic,GITHUB_RUN_ID:'123',GITHUB_RUN_ATTEMPT:'1',SHA:sha}
  });
  assert.equal(tag, `verify-123-1-${sha}`);
  console.log('PASS manual gate and actual tag selection produce verify-only tag');
  for (const current of [sha, 'c'.repeat(40)]) {
    const calls = [];
    await runPromotion({repo:{owner:'leriusss',repo:'tg-spam'}}, {
      rest:{git:{getRef:async args=>{assert.equal(args.ref,'heads/master'); calls.push('getRef');return {data:{object:{sha:current}}};}}}
    }, {notice:()=>calls.push('stale')}, {
      exec:async (cmd,args)=>{calls.push('write');assert.equal(cmd,'docker');assert.deepEqual(args,
        ['buildx','imagetools','create','-t',`${image}:master`,`${image}@${digest}`]);}
    }, {env:{SHA:sha,IMAGE:image,DIGEST:digest}});
    assert.deepEqual(calls, current === sha ? ['getRef','write'] : ['getRef','stale']);
    console.log(current === sha ? 'PASS current tip writes the checked digest immediately after SHA lookup' : 'PASS stale SHA leaves master unchanged');
  }
})().catch(e=>{console.error(e); process.exitCode=1;});
