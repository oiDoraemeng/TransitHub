import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import crypto from 'node:crypto';
import zlib from 'node:zlib';
import {fileURLToPath,pathToFileURL} from 'node:url';

const source = path.resolve(process.argv[2]);
const target = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../backend/internal/modules/model_proxy/testdata/fingerprint_templates.json.gz');
const ctx = vm.createContext({crypto:crypto.webcrypto});
vm.runInContext(fs.readFileSync(path.join(source,'upstream-challenge-browser.mjs'),'utf8').replace('export function generateChallenges','function generateChallenges')+'\nthis.templates=LANGUAGES;',ctx);
const families = Object.entries(ctx.templates).map(([id,t])=>({id:'ikale-'+id,...t,request:t.request('@action@',987654321),min:292,max:332}));
const mt = JSON.parse(fs.readFileSync(path.join(source,'modeltrace-compatibility/live-template-data.json'),'utf8'));
families.push({id:'modeltrace-live',space:'',...mt.t,rules:mt.sharedNoPattern+mt.sharedDirect+mt.sharedTools,start:mt.start,min:308,max:342});
const plugin = await import(pathToFileURL(path.join(source,'modeltrace-compatibility/codex-plugin/modeltrace-guard/scripts/prompts.mjs')));
const guard = [];
for(const language of plugin.LANGUAGES) for(let n=292;n<=332;n++) guard.push({text:plugin.forkPrompt(language,n),count:n,accept:true},{text:plugin.localizedPrompt(language,n,'node /audit/guard.mjs submit --numbers [YOUR_LITERAL_INTEGERS]'),count:n,accept:false});
fs.writeFileSync(target,zlib.gzipSync(JSON.stringify({families,guard})));
console.log(JSON.stringify({families:families.length,guard:guard.length}));
