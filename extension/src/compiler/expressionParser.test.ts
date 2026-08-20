/**
 * Comprehensive test suite for the expression parser.
 */

import { inferExpressionType } from './expressionParser';
import { TemplateVar, ScopeFrame, FieldInfo } from '../types';

// ── Test Data Setup ────────────────────────────────────────────────────────

function createTestVars(): Map<string, TemplateVar> {
  return new Map([
    ['User', {
      name: 'User',
      type: 'User',
      isSlice: false,
      fields: [
        { name: 'Name', type: 'string', isSlice: false },
        { name: 'Age', type: 'int', isSlice: false },
        { name: 'Email', type: 'string', isSlice: false },
        {
          name: 'Profile',
          type: 'Profile',
          isSlice: false,
          fields: [
            { name: 'Bio', type: 'string', isSlice: false },
            { name: 'Avatar', type: 'string', isSlice: false },
          ],
        },
        // Added for function/method testing
        { name: 'GetAge', type: 'func() int', isSlice: false },
        { name: 'GetProfile', type: 'func() Profile', isSlice: false },
        {
          name: 'HasRole',
          type: 'method',
          isSlice: false,
          returns: [{ type: 'bool', name: '' }]
        },
      ],
    }],
    ['Items', {
      name: 'Items',
      type: '[]Item',
      isSlice: true,
      elemType: 'Item',
      fields: [
        { name: 'Name', type: 'string', isSlice: false },
        { name: 'Price', type: 'float64', isSlice: false },
        { name: 'Quantity', type: 'int', isSlice: false },
        {
          name: 'Tags',
          type: '[]string',
          isSlice: true,
          elemType: 'string',
        },
        // Added for function testing
        { name: 'GetPrice', type: 'func() float64', isSlice: false },
      ],
    }],
    ['Count', {
      name: 'Count',
      type: 'int',
      isSlice: false,
    }],
    ['Total', {
      name: 'Total',
      type: 'float64',
      isSlice: false,
    }],
    ['Active', {
      name: 'Active',
      type: 'bool',
      isSlice: false,
    }],
    ['Config', {
      name: 'Config',
      type: 'map[string]interface{}',
      isMap: true,
      keyType: 'string',
      elemType: 'interface{}',
      isSlice: false,
    }],
    ['Settings', {
      name: 'Settings',
      type: 'map[string]string',
      isMap: true,
      keyType: 'string',
      elemType: 'string',
      isSlice: false,
    }],
    ['NestedConfig', {
      name: 'NestedConfig',
      type: 'map[string]map[string]Item',
      isMap: true,
      keyType: 'string',
      elemType: 'map[string]Item',
      isSlice: false,
    }],
    ['paymentsMap', {
      name: 'paymentsMap',
      type: 'map[uint][]*Payment',
      isMap: true,
      keyType: 'uint',
      elemType: '[]*Payment',
      isSlice: false,
    }],
    ['visitIDS', {
      name: 'visitIDS',
      type: '[]uint',
      isSlice: true,
      elemType: 'uint',
    }],
  ]);
}

// ── Shared Test Case Shape ─────────────────────────────────────────────────

interface TestCase {
  name: string;
  expr: string;
  expectedType: string;
  expectedSlice?: boolean;
  expectedMap?: boolean;
  scope?: ScopeFrame[];
  blockLocals?: Map<string, TemplateVar>;
}

/**
 * Runs inferExpressionType for a single test case and asserts the result
 * against the case's expectations. Failures surface through Jest's own
 * matcher diffs, so no manual "Expected/Got" logging is needed here.
 * @param tc The test case to evaluate.
 * @param vars The base template variable map (shared fixture across cases).
 * @param fieldResolver Optional field resolver, used only by the pointer/slice/map suite.
 */
function assertExpressionType(
  tc: TestCase,
  vars: Map<string, TemplateVar>,
  fieldResolver?: (typeStr: string) => FieldInfo[] | undefined
): void {
  const result = inferExpressionType(
    tc.expr,
    vars,
    tc.scope || [],
    tc.blockLocals,
    undefined, // funcMaps
    fieldResolver
  );

  expect(result).not.toBeNull();
  expect(result!.typeStr).toBe(tc.expectedType);
  if (tc.expectedSlice !== undefined) {
    expect(result!.isSlice).toBe(tc.expectedSlice);
  }
  if (tc.expectedMap !== undefined) {
    expect(result!.isMap).toBe(tc.expectedMap);
  }
}

// ── Shared Fixture ──────────────────────────────────────────────────────────

let vars: Map<string, TemplateVar>;

beforeEach(() => {
  vars = createTestVars();
});

// ── Suite 1: Basic Field Access ─────────────────────────────────────────────

describe('Basic Field Access', () => {
  const tests: TestCase[] = [
    { name: 'Bare dot', expr: '.', expectedType: 'context' },
    { name: 'Simple field', expr: '.Count', expectedType: 'int' },
    { name: 'Nested field', expr: '.User.Name', expectedType: 'string' },
    { name: 'Deep nested field', expr: '.User.Profile.Bio', expectedType: 'string' },
    { name: 'Root context', expr: '$', expectedType: 'context' },
    { name: 'Root field access', expr: '$.Count', expectedType: 'int' },
    { name: 'Slice field', expr: '.Items', expectedType: '[]Item', expectedSlice: true },
    { name: 'Map field', expr: '.Config', expectedType: 'map[string]interface{}', expectedMap: true },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 2: Built-in Functions ──────────────────────────────────────────────

describe('Built-in Functions', () => {
  const tests: TestCase[] = [
    { name: 'len on slice', expr: 'len .Items', expectedType: 'int' },
    { name: 'len on map', expr: 'len .Config', expectedType: 'int' },
    { name: 'index on slice', expr: 'index .Items 0', expectedType: 'Item' },
    { name: 'index on map', expr: 'index .Config "key"', expectedType: 'interface{}' },
    { name: 'index on map multiple keys', expr: 'index .NestedConfig "key1" "key2"', expectedType: 'Item' },
    { name: 'slice operation', expr: 'slice .Items 0 5', expectedType: '[]Item' },
    { name: 'print function', expr: 'print .Count', expectedType: 'string' },
    { name: 'printf function', expr: 'printf "%d" .Count', expectedType: 'string' },
    { name: 'println function', expr: 'println .Count', expectedType: 'string' },
    { name: 'html escape', expr: 'html .User.Profile.Bio', expectedType: 'string' },
    { name: 'js escape', expr: 'js .User.Name', expectedType: 'string' },
    { name: 'urlquery escape', expr: 'urlquery .User.Email', expectedType: 'string' },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 3: Comparison Operations ──────────────────────────────────────────

describe('Comparison Operations', () => {
  const tests: TestCase[] = [
    { name: 'eq comparison', expr: 'eq .Count 10', expectedType: 'bool' },
    { name: 'ne comparison', expr: 'ne .Count 0', expectedType: 'bool' },
    { name: 'lt comparison', expr: 'lt .Count 100', expectedType: 'bool' },
    { name: 'le comparison', expr: 'le .Count 100', expectedType: 'bool' },
    { name: 'gt comparison', expr: 'gt .Count 0', expectedType: 'bool' },
    { name: 'ge comparison', expr: 'ge .Count 0', expectedType: 'bool' },
    { name: 'eq with string', expr: 'eq .User.Name "admin"', expectedType: 'bool' },
    { name: 'nested eq', expr: 'eq (len .Items) 5', expectedType: 'bool' },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 4: Logical Operations ─────────────────────────────────────────────

describe('Logical Operations', () => {
  const tests: TestCase[] = [
    { name: 'not operation', expr: 'not .Active', expectedType: 'bool' },
    { name: 'and operation', expr: 'and .Active (gt .Count 0)', expectedType: 'bool' },
    { name: 'or operation', expr: 'or .Active (eq .Count 0)', expectedType: 'bool' },
    { name: 'nested and', expr: 'and (gt .Count 0) (lt .Count 100)', expectedType: 'bool' },
    { name: 'nested or', expr: 'or (eq .Count 0) (eq .Count -1)', expectedType: 'bool' },
    { name: 'complex logical', expr: 'and (not .Active) (gt .Count 5)', expectedType: 'bool' },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 5: Pipeline Operations ────────────────────────────────────────────

describe('Pipeline Operations', () => {
  const tests: TestCase[] = [
    { name: 'Simple pipe', expr: '.Count | printf "%d"', expectedType: 'string' },
    { name: 'Multi-stage pipe', expr: '.Items | len | printf "%d"', expectedType: 'string' },
    { name: 'Pipe with comparison', expr: '.Count | gt 10', expectedType: 'bool' },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 6: Collection Operations ──────────────────────────────────────────

describe('Collection Operations', () => {
  const tests: TestCase[] = [
    { name: 'Slice access', expr: 'index .Items 0', expectedType: 'Item' },
    { name: 'Slice length', expr: 'len .Items', expectedType: 'int' },
    { name: 'Slice operation', expr: 'slice .Items 1 3', expectedType: '[]Item' },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 7: Map Operations ─────────────────────────────────────────────────

describe('Map Operations', () => {
  const tests: TestCase[] = [
    { name: 'Map index', expr: 'index .Config "key"', expectedType: 'interface{}' },
    { name: 'Map length', expr: 'len .Config', expectedType: 'int' },
    { name: 'String map index', expr: 'index .Settings "theme"', expectedType: 'string' },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 8: Scope and Context ──────────────────────────────────────────────

describe('Scope and Context', () => {
  const itemFields: FieldInfo[] = [
    { name: 'Name', type: 'string', isSlice: false },
    { name: 'Price', type: 'float64', isSlice: false },
  ];

  const scope: ScopeFrame[] = [
    {
      key: '.',
      typeStr: 'Item',
      fields: itemFields,
    },
  ];

  const tests: TestCase[] = [
    { name: 'Scoped field access', expr: '.Name', expectedType: 'string', scope },
    { name: 'Scoped nested access', expr: '.Price', expectedType: 'float64', scope },
    { name: 'Root access inside scope', expr: '$.User.Name', expectedType: 'string', scope },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 9: Local Variables ────────────────────────────────────────────────

describe('Local Variables', () => {
  const blockLocals = new Map<string, TemplateVar>([
    ['$item', {
      name: '$item',
      type: 'Item',
      isSlice: false,
      fields: [
        { name: 'Name', type: 'string', isSlice: false },
        { name: 'Price', type: 'float64', isSlice: false },
      ],
    }],
    ['$idx', { name: '$idx', type: 'int', isSlice: false }],
  ]);

  const scopeWithLocals: ScopeFrame[] = [
    {
      key: '.',
      typeStr: 'context',
      locals: new Map([
        ['$parentVar', { name: '$parentVar', type: 'string', isSlice: false }]
      ])
    }
  ];

  const tests: TestCase[] = [
    { name: 'Simple local var', expr: '$item', expectedType: 'Item', blockLocals },
    { name: 'Local var field access', expr: '$item.Name', expectedType: 'string', blockLocals },
    { name: 'Local var in function', expr: 'index .Items $idx', expectedType: 'Item', blockLocals },
    { name: 'Local var comparison', expr: 'gt $item.Price 10.0', expectedType: 'bool', blockLocals },
    { name: 'Parent scope local var', expr: '$parentVar', expectedType: 'string', scope: scopeWithLocals },
    { name: 'Mix root and local', expr: 'eq $.Count $idx', expectedType: 'bool', blockLocals },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 10: Complex Expressions ───────────────────────────────────────────

describe('Complex Expressions', () => {
  const tests: TestCase[] = [
    {
      name: 'Nested function calls',
      expr: 'printf "%d items" (len .Items)',
      expectedType: 'string',
    },
    {
      name: 'Multiple comparisons',
      expr: 'and (gt .Count 0) (le .Count 100)',
      expectedType: 'bool',
    },
    {
      name: 'Pipeline with function',
      expr: '.Items | len | printf "Count: %d"',
      expectedType: 'string',
    },
    {
      name: 'Comparison in pipeline',
      expr: '.Count | gt 10',
      expectedType: 'bool',
    },
    {
      name: 'Complex logical with fields',
      expr: 'and (eq .User.Profile.Bio "") (not .Active)',
      expectedType: 'bool',
    },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 11: Function and Method Calls ─────────────────────────────────────

describe('Function and Method Calls', () => {
  const blockLocals = new Map<string, TemplateVar>([
    ['$role', { name: '$role', type: 'string', isSlice: false }],
    ['$getDiscount', { name: '$getDiscount', type: 'func() float64', isSlice: false }],
  ]);

  const itemScope: ScopeFrame[] = [
    {
      key: '.',
      typeStr: 'Item',
      fields: [
        { name: 'Name', type: 'string', isSlice: false },
        { name: 'GetPrice', type: 'func() float64', isSlice: false },
      ],
    },
  ];

  const tests: TestCase[] = [
    // 1. Struct fields that are functions (unwrapping)
    { name: 'Unwrap func field', expr: '.User.GetAge', expectedType: 'int' },
    { name: 'Unwrap func field returning struct', expr: '.User.GetProfile', expectedType: 'Profile' },
    { name: 'Unwrap func field in dot scope', expr: '.GetPrice', expectedType: 'float64', scope: itemScope },

    // 2. The `call` keyword evaluating its target
    { name: 'Call with func field', expr: 'call .User.GetAge', expectedType: 'int' },
    { name: 'Call with dot scope func field', expr: 'call .GetPrice', expectedType: 'float64', scope: itemScope },
    { name: 'Call with local func var', expr: 'call $getDiscount', expectedType: 'float64', blockLocals },

    // 3. DOLLAR token in method arguments
    { name: 'Method call with variable arg', expr: '.User.HasRole $role', expectedType: 'bool', blockLocals },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 12: Edge Cases ─────────────────────────────────────────────────────

describe('Edge Cases', () => {
  const tests: TestCase[] = [
    { name: 'String literal', expr: '"hello world"', expectedType: 'string' },
    { name: 'Number literal', expr: '42', expectedType: 'int' },
    { name: 'Parenthesized expression', expr: '(gt .Count 5)', expectedType: 'bool' },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars);
  });
});

// ── Suite 13: Pointer Slice Map Operations ──────────────────────────────────

describe('Pointer Slice Map Operations', () => {
  // Simulates:
  //   {{ range .visitIDS }}
  //     {{ $visitId := . }}
  //     {{ $paymentSlice := index $.paymentsMap . }}
  //     {{ $firstPayment := index $paymentSlice 0 }}
  //     {{ $lastPayment := index $paymentSlice (minus (len $paymentSlice) 1) }}
  //   {{ end }}

  // fieldResolver that knows Payment has Amount and Reference fields.
  const paymentFields: FieldInfo[] = [
    { name: 'Amount', type: 'float64', isSlice: false },
    { name: 'Reference', type: 'string', isSlice: false },
  ];
  const fieldResolver = (typeStr: string): FieldInfo[] | undefined => {
    if (typeStr === 'Payment') return paymentFields;
    return undefined;
  };

  // After `$paymentSlice := index $.paymentsMap .` the local is []*Payment.
  const paymentSliceLocal = new Map<string, TemplateVar>([
    ['$paymentSlice', {
      name: '$paymentSlice',
      type: '[]*Payment',
      isSlice: true,
      elemType: 'Payment', // pointer already stripped by the inferencer
    }],
  ]);

  // After `$firstPayment := index $paymentSlice 0` the local is Payment.
  const paymentItemLocal = new Map<string, TemplateVar>([
    ['$paymentSlice', {
      name: '$paymentSlice',
      type: '[]*Payment',
      isSlice: true,
      elemType: 'Payment',
    }],
    ['$firstPayment', {
      name: '$firstPayment',
      type: 'Payment',
      isSlice: false,
      fields: paymentFields,
    }],
  ]);

  const tests: TestCase[] = [
    // map[uint][]*Payment — indexing yields []*Payment
    {
      name: 'index map[uint][]*Payment → []*Payment',
      expr: 'index $.paymentsMap .',
      expectedType: '[]*Payment',
      expectedSlice: true,
      expectedMap: false,
    },
    // []*Payment — indexing yields Payment (pointer stripped)
    {
      name: 'index []*Payment → Payment',
      expr: 'index $paymentSlice 0',
      expectedType: 'Payment',
      expectedSlice: false,
      blockLocals: paymentSliceLocal,
    },
    // len of []*Payment → int
    {
      name: 'len []*Payment → int',
      expr: 'len $paymentSlice',
      expectedType: 'int',
      blockLocals: paymentSliceLocal,
    },
    // slice of []*Payment → []*Payment
    {
      name: 'slice []*Payment → []*Payment',
      expr: 'slice $paymentSlice 0 2',
      expectedType: '[]*Payment',
      expectedSlice: true,
      blockLocals: paymentSliceLocal,
    },
    // field access on indexed Payment
    {
      name: 'field access on Payment from index',
      expr: '$firstPayment.Amount',
      expectedType: 'float64',
      blockLocals: paymentItemLocal,
    },
    {
      name: 'field access Reference on Payment from index',
      expr: '$firstPayment.Reference',
      expectedType: 'string',
      blockLocals: paymentItemLocal,
    },
  ];

  test.each(tests)('$name', (tc) => {
    assertExpressionType(tc, vars, fieldResolver);
  });
});
