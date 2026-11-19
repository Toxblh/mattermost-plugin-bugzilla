import 'fake-indexeddb/auto';
import {describe,it,expect} from 'vitest';
import {CardCache,valid} from './cache';
import {Card,formatLinks,parseReference} from './types';
const base='https://bugzilla.altlinux.org';
const card={bug:{id:123,summary:'Private [title](https://evil.test)',status:'NEW'},revision:'r1',source_fetched_at:new Date().toISOString(),max_age_ms:1000} as Card;
describe('link parsing and formatting',()=>{
 it('normalizes short links and local comment numbers',()=>{expect(parseReference(`${base}/123`,base)?.id).toBe(123);expect(parseReference(`${base}/show_bug.cgi?id=123#c15`,base)?.comment).toBe(15);expect(parseReference(`${base}.evil/123`,base)).toBeNull();expect(parseReference(`${base}/show_bug.cgi?id=1&id=2`,base)).toBeNull();});
 it('never edits code or explicit Markdown link labels',()=>{for(const text of [`\`${base}/123\``,`[mine](${base}/123)`,`\`\`\`\n${base}/123\n\`\`\``])expect(formatLinks(text,base,()=>card)).toBe(text);});
 it('escapes titles, preserves destination and does not mutate message',()=>{const message=`See ${base}/123.`;const result=formatLinks(message,base,()=>card);expect(message).toBe(`See ${base}/123.`);expect(result).toContain('\\[title\\]');expect(result).toContain(`](${base}/123).`);});
});
describe('client cache',()=>{
 it('does not extend remaining freshness and persists between instances',async()=>{const c=new CardCache(),owner='test-user-a',key=owner+'|personal|r1|123';await c.put(key,owner,card);const fresh=new CardCache();expect((await fresh.get(key))?.bug.id).toBe(123);await fresh.clearOwner(owner);expect(await new CardCache().get(key)).toBeUndefined();});
 it('separates accounts and clears only the logged-out owner',async()=>{const c=new CardCache();await c.put('alice|r1|123','alice',card);await c.put('bob|r2|123','bob',card);await c.clearOwner('alice');expect(await c.get('alice|r1|123')).toBeUndefined();expect(await c.get('bob|r2|123')).toBeDefined();await c.clearOwner('bob');});
 it('rejects expiry and backward clock changes',()=>{const e={key:'a',owner:'a',value:card,savedAt:100,expiresAt:200};expect(valid(e,199)).toBe(true);expect(valid(e,200)).toBe(false);expect(valid(e,99)).toBe(false);});
});
