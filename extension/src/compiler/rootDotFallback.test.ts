import { resolvePath } from '../templateParser';
import { TemplateVar } from '../types';

function makeVisitVar(): TemplateVar {
  return {
    name: '.',
    type: '*handlers.Visit',
    isSlice: false,
    fields: [
      { name: 'Patient', type: '*handlers.Patient', isSlice: false,
        fields: [
          { name: 'Name', type: 'string', isSlice: false, defFile: 'handlers.go', defLine: 42, defCol: 3 },
          { name: 'Age', type: 'int', isSlice: false },
        ] },
      { name: 'Status', type: 'string', isSlice: false },
    ],
  };
}

const resolver = (typeStr: string) => {
  const map: Record<string, any[]> = {
    '*handlers.Patient': [
      { name: 'Name', type: 'string', isSlice: false, defFile: 'handlers.go', defLine: 42, defCol: 3 },
      { name: 'Age', type: 'int', isSlice: false },
    ],
  };
  return map[typeStr];
};

function check(name: string, cond: boolean) {
  if (cond) console.log(`PASS ${name}`);
  else { console.log(`FAIL ${name}`); process.exitCode = 1; }
}

const vars = new Map<string, TemplateVar>([['.', makeVisitVar()]]);

// 1. Root dot with sub-path (empty scope stack)
let r = resolvePath(['.', 'Patient', 'Name'], vars, [], undefined, resolver);
check('root .Patient.Name found', r.found);
check('root .Patient.Name type', r.typeStr === 'string');

// 2. Bare root dot
r = resolvePath(['.'], vars, [], undefined, resolver);
check('bare root . found', r.found);
check('bare root . fields has Patient', Array.isArray(r.fields) && r.fields!.some((f: any) => f.name === 'Patient'));

// 3. With a range/with dot frame present, root fallback must NOT shadow it
const stack = [{ key: '.', typeStr: 'string', fields: [] }];
r = resolvePath(['.', 'Status'], vars, stack, undefined, resolver);
check('dot frame still takes precedence', r.found && r.typeStr === 'string');

// 4. No root dot var → old behavior (unknown not found for .sub)
const empty = new Map<string, TemplateVar>();
r = resolvePath(['.', 'Status'], empty, [], undefined, resolver);
check('no root dot → not found for .sub', !r.found);