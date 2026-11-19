import {Card,TTL} from './types';
export interface Entry {key:string;owner:string;value:Card;savedAt:number;expiresAt:number}
const DB='space.altlinux.bugzilla.cards.v1';
export function valid(e:Entry,now=Date.now()):boolean{return now>=e.savedAt&&now<e.expiresAt&&e.expiresAt-e.savedAt<=TTL;}
export class CardCache {
 memory=new Map<string,Entry>();private db?:Promise<IDBDatabase|null>;
 private open():Promise<IDBDatabase|null>{if(!this.db)this.db=new Promise(resolve=>{
  if(typeof indexedDB==='undefined'){resolve(null);return;}const req=indexedDB.open(DB,1);req.onupgradeneeded=()=>{req.result.createObjectStore('cards',{keyPath:'key'});};req.onsuccess=()=>resolve(req.result);req.onerror=()=>resolve(null);req.onblocked=()=>resolve(null);
 });return this.db;}
 peek(key:string):Card|undefined{const e=this.memory.get(key);if(e&&valid(e))return e.value;if(e)this.memory.delete(key);return undefined;}
 async get(key:string):Promise<Card|undefined>{const hit=this.peek(key);if(hit)return hit;const db=await this.open();if(!db)return undefined;
 return new Promise(resolve=>{const tx=db.transaction('cards','readonly');const req=tx.objectStore('cards').get(key);req.onsuccess=()=>{const e=req.result as Entry|undefined;if(e&&valid(e)){this.memory.set(key,e);resolve(e.value);}else resolve(undefined);};req.onerror=()=>resolve(undefined);tx.onabort=()=>resolve(undefined);});}
 async put(key:string,owner:string,value:Card):Promise<void>{const now=Date.now();const expiresAt=now+Math.max(0,Math.min(TTL,value.max_age_ms));const e:Entry={key,owner,value:{...value,client_expires_at:expiresAt},savedAt:now,expiresAt};this.memory.set(key,e);if(this.memory.size>500)this.memory.delete(this.memory.keys().next().value!);const db=await this.open();if(!db)return;await new Promise<void>(resolve=>{const tx=db.transaction('cards','readwrite');tx.objectStore('cards').put(e);tx.oncomplete=()=>resolve();tx.onerror=()=>resolve();tx.onabort=()=>resolve();});await this.prune(db);}
 async clearOwner(owner:string):Promise<void>{for(const [k,e]of this.memory)if(e.owner===owner)this.memory.delete(k);const db=await this.open();if(!db)return;await new Promise<void>(resolve=>{const tx=db.transaction('cards','readwrite');const r=tx.objectStore('cards').openCursor();r.onsuccess=()=>{const c=r.result;if(c){if(c.value.owner===owner)c.delete();c.continue();}};tx.oncomplete=()=>resolve();tx.onerror=()=>resolve();tx.onabort=()=>resolve();});}
 async remove(key:string):Promise<void>{this.memory.delete(key);const db=await this.open();if(!db)return;await new Promise<void>(resolve=>{const tx=db.transaction('cards','readwrite');tx.objectStore('cards').delete(key);tx.oncomplete=()=>resolve();tx.onerror=()=>resolve();tx.onabort=()=>resolve();});}
 private async prune(db:IDBDatabase):Promise<void>{await new Promise<void>(resolve=>{const tx=db.transaction('cards','readwrite'),s=tx.objectStore('cards'),r=s.getAll();r.onsuccess=()=>{const rows=(r.result as Entry[]).sort((a,b)=>b.savedAt-a.savedAt);rows.forEach((e,i)=>{if(!valid(e)||i>=500)s.delete(e.key);});};tx.oncomplete=()=>resolve();tx.onerror=()=>resolve();tx.onabort=()=>resolve();});}
}
