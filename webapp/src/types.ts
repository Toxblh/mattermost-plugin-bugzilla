export const ID = 'space.altlinux.bugzilla';
export const TTL = 300_000;
export interface Configuration {url:string;instance:string;revision:string;connected:boolean;can_connect:boolean}
export interface Bug {id:number;summary:string;status:string;resolution:string;priority:string;assigned_to:string;product:string;component:string;last_change_time:string}
export interface Comment {id:number;count:number;text:string;creator:string;creation_time:string;is_private:boolean}
export interface Card {client_expires_at?:number;bug:Bug;comment?:Comment;description?:string;url:string;mode:'public'|'personal';revision:string;source_fetched_at:string;max_age_ms:number}
export interface Reference {id:number;comment?:number;href:string}
export function parseReference(raw:string,base:string):Reference|null {
 try {const u=new URL(raw), b=new URL(base);if(u.origin!==b.origin||u.username||u.password)return null;
 const prefix=b.pathname.replace(/\/$/,'');if(!u.pathname.startsWith(`${prefix}/`))return null;const path=u.pathname.slice(prefix.length);
 const n=path==='/show_bug.cgi'?(u.searchParams.getAll('id').length===1?u.searchParams.get('id'):null):path.slice(1);
 if(!n||!/^\d+$/.test(n)||!Number.isSafeInteger(Number(n))||Number(n)<=0||String(Number(n))!==n)return null;
 let comment:number|undefined;if(u.hash.startsWith('#c')){const v=u.hash.slice(2);if(!/^\d+$/.test(v)||!Number.isSafeInteger(Number(v)))return null;comment=Number(v);}
 return {id:Number(n),comment,href:raw};
 }catch{return null;}
}
export function escapeMarkdown(s:string):string {return s.replace(/[\r\n]+/g,' ').replace(/[\\`*_{}\[\]()<>#!|~]/g,'\\$&');}
export function formatLinks(message:string,base:string,lookup:(r:Reference)=>Card|undefined):string {
 // Never transform code or existing Markdown links. Only return a rendered string,
 // never mutate the stored/shared post or send the resolved title to Mattermost.
 return message.split(/(```[\s\S]*?```|`[^`\n]*`|\[[^\]\n]*\]\([^\s)]*\))/g).map((part,i)=>i%2?part:part.replace(/https?:\/\/[^\s<>()[\]]+/g,(raw)=>{
  const trailing=raw.match(/[.,;!?]+$/)?.[0]||'';const href=trailing?raw.slice(0,-trailing.length):raw;const ref=parseReference(href,base);if(!ref)return raw;
  const card=lookup(ref);const title=card?` · ${card.bug.summary.slice(0,85)} · ${card.bug.status}`:'';return `[${escapeMarkdown(`🐞 BZ #${ref.id}${title}`)}](${href})${trailing}`;
 })).join('');
}
