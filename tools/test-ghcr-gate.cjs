const assert = require('node:assert/strict');
// Run: python -c "import yaml,json; print(json.dumps(yaml.safe_load(open('.github/workflows/docker.yml'))['jobs']['gate']['steps'][0]['with']['script']))" | node tools/test-ghcr-gate.cjs
const source = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const gate = new (Object.getPrototypeOf(async function(){}).constructor)('context','github','core', source);
const sha='a'.repeat(40), full='leriusss/tg-spam';
const base={conclusion:'success',event:'push',name:'build-app',path:'.github/workflows/ci.yml',repository:{full_name:full},head_repository:{full_name:full},head_branch:'master',pull_requests:[],head_sha:sha};
async function check(label, event, payload, expected, runs=[]) {
 const out={}; let failed=false;
 try {await gate({repo:{owner:'leriusss',repo:'tg-spam'},sha,eventName:event,payload},
 {paginate:async()=>runs,rest:{actions:{listWorkflowRuns(){}}}}, {setOutput:(k,v)=>out[k]=v});} catch(e){failed=true;}
 if(expected==='reject') assert(failed); else {assert(!failed); assert.equal(out.publish,expected);}
 console.log('PASS '+label);
}
(async()=>{
 await check('master push','workflow_run',{workflow_run:base},'true');
 for(const [label,change] of [
 ['PR',{event:'pull_request'}],['fork PR',{event:'pull_request',head_repository:{full_name:'fork/tg-spam'}}],
 ['failed CI',{conclusion:'failure'}],['v branch',{head_branch:'v1'}],
 ['wrong repository',{repository:{full_name:'other/tg-spam'}}],['fork push',{head_repository:{full_name:'other/tg-spam'}}],
 ['wrong workflow',{path:'.github/workflows/other.yml'}]]) {
 await check(label,'workflow_run',{workflow_run:{...base,...change}},undefined);
 }
 await check('manual default','workflow_dispatch',{inputs:{mode:'build-only'}},'false');
 await check('manual publish without CI','workflow_dispatch',{inputs:{mode:'verify-publish'}},'reject');
 await check('manual publish exact successful push CI','workflow_dispatch',{inputs:{mode:'verify-publish'}},'true',[base]);
 await check('manual publish wrong SHA','workflow_dispatch',{inputs:{mode:'verify-publish'}},'reject',[{...base,head_sha:'b'.repeat(40)}]);
})();
