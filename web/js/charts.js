// Small hand-built SVG charts. The dashboard needs three shapes, and drawing
// them directly keeps the frontend free of a charting dependency.

const NS = 'http://www.w3.org/2000/svg';

function svgEl(tag, attrs = {}) {
  const node = document.createElementNS(NS, tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === null || value === undefined) continue;
    node.setAttribute(key, String(value));
  }
  return node;
}

const PALETTE = {
  accent: '#059669',
  info: '#2563eb',
  warn: '#f59e0b',
  danger: '#d92d20',
  muted: '#98a2b3',
};

export const chartColors = PALETTE;

// lineChart draws one or more series over a shared date axis.
export function lineChart(series, { height = 220, showArea = true } = {}) {
  const width = 720;
  const pad = { top: 14, right: 12, bottom: 24, left: 34 };

  const svg = svgEl('svg', {
    class: 'chart',
    viewBox: `0 0 ${width} ${height}`,
    preserveAspectRatio: 'none',
    role: 'img',
  });

  const points = series[0]?.points || [];
  if (points.length === 0) {
    svg.append(svgEl('text', {
      x: width / 2, y: height / 2, 'text-anchor': 'middle', class: 'chart__axis',
    })).lastChild.textContent = 'Дерек жоқ';
    return svg;
  }

  const maxValue = Math.max(1, ...series.flatMap((s) => s.points.map((p) => p.value)));
  const plotW = width - pad.left - pad.right;
  const plotH = height - pad.top - pad.bottom;

  const x = (index) => pad.left + (points.length === 1
    ? plotW / 2
    : (index / (points.length - 1)) * plotW);
  const y = (value) => pad.top + plotH - (value / maxValue) * plotH;

  // Horizontal grid with four bands keeps the chart readable without clutter.
  for (let i = 0; i <= 4; i += 1) {
    const gy = pad.top + (plotH / 4) * i;
    svg.append(svgEl('line', {
      x1: pad.left, y1: gy, x2: width - pad.right, y2: gy, class: 'chart__grid',
    }));

    const label = svgEl('text', {
      x: pad.left - 6, y: gy + 3, 'text-anchor': 'end', class: 'chart__axis',
    });
    label.textContent = String(Math.round(maxValue - (maxValue / 4) * i));
    svg.append(label);
  }

  for (const s of series) {
    const color = PALETTE[s.color] || s.color || PALETTE.accent;
    const path = s.points.map((p, i) => `${i === 0 ? 'M' : 'L'}${x(i).toFixed(1)},${y(p.value).toFixed(1)}`).join(' ');

    if (showArea) {
      svg.append(svgEl('path', {
        d: `${path} L${x(s.points.length - 1).toFixed(1)},${pad.top + plotH} L${x(0).toFixed(1)},${pad.top + plotH} Z`,
        fill: color,
        class: 'chart__area',
      }));
    }

    svg.append(svgEl('path', { d: path, stroke: color, class: 'chart__line' }));

    // Only mark the final point; a dot per day is noise at 30 days.
    const last = s.points[s.points.length - 1];
    svg.append(svgEl('circle', {
      cx: x(s.points.length - 1), cy: y(last.value), r: 3.5,
      fill: color, class: 'chart__dot',
    }));
  }

  // Date labels: first, middle and last, so they never overlap.
  const labelIndices = points.length > 2
    ? [0, Math.floor(points.length / 2), points.length - 1]
    : points.map((_, i) => i);

  for (const index of labelIndices) {
    const label = svgEl('text', {
      x: x(index),
      y: height - 6,
      'text-anchor': index === 0 ? 'start' : index === points.length - 1 ? 'end' : 'middle',
      class: 'chart__axis',
    });
    label.textContent = shortDate(points[index].date);
    svg.append(label);
  }

  return svg;
}

export function barChart(points, { height = 220, color = 'accent' } = {}) {
  const width = 720;
  const pad = { top: 14, right: 12, bottom: 26, left: 34 };

  const svg = svgEl('svg', {
    class: 'chart', viewBox: `0 0 ${width} ${height}`,
    preserveAspectRatio: 'none', role: 'img',
  });

  if (points.length === 0) return svg;

  const maxValue = Math.max(1, ...points.map((p) => p.value));
  const plotW = width - pad.left - pad.right;
  const plotH = height - pad.top - pad.bottom;
  const slot = plotW / points.length;
  const barW = Math.max(2, Math.min(24, slot * 0.62));

  for (let i = 0; i <= 4; i += 1) {
    const gy = pad.top + (plotH / 4) * i;
    svg.append(svgEl('line', { x1: pad.left, y1: gy, x2: width - pad.right, y2: gy, class: 'chart__grid' }));
    const label = svgEl('text', { x: pad.left - 6, y: gy + 3, 'text-anchor': 'end', class: 'chart__axis' });
    label.textContent = String(Math.round(maxValue - (maxValue / 4) * i));
    svg.append(label);
  }

  points.forEach((point, index) => {
    const barH = (point.value / maxValue) * plotH;
    const bx = pad.left + slot * index + (slot - barW) / 2;

    const rect = svgEl('rect', {
      x: bx, y: pad.top + plotH - barH, width: barW, height: Math.max(barH, point.value > 0 ? 2 : 0),
      fill: PALETTE[color] || color, class: 'chart__bar',
    });
    const title = svgEl('title');
    title.textContent = `${point.label || point.date}: ${point.value}`;
    rect.append(title);
    svg.append(rect);
  });

  const labelIndices = points.length > 2
    ? [0, Math.floor(points.length / 2), points.length - 1]
    : points.map((_, i) => i);

  for (const index of labelIndices) {
    const label = svgEl('text', {
      x: pad.left + slot * index + slot / 2,
      y: height - 8,
      'text-anchor': 'middle',
      class: 'chart__axis',
    });
    label.textContent = points[index].label || shortDate(points[index].date);
    svg.append(label);
  }

  return svg;
}

// donut renders a compact status breakdown.
export function donut(segments, { size = 168, thickness = 20 } = {}) {
  const svg = svgEl('svg', {
    width: size, height: size, viewBox: `0 0 ${size} ${size}`, role: 'img',
  });

  const total = segments.reduce((sum, s) => sum + s.value, 0);
  const radius = (size - thickness) / 2;
  const center = size / 2;
  const circumference = 2 * Math.PI * radius;

  svg.append(svgEl('circle', {
    cx: center, cy: center, r: radius,
    fill: 'none', stroke: '#f2f4f7', 'stroke-width': thickness,
  }));

  if (total === 0) return svg;

  let offset = 0;
  for (const segment of segments) {
    if (segment.value <= 0) continue;

    const length = (segment.value / total) * circumference;
    const arc = svgEl('circle', {
      cx: center, cy: center, r: radius,
      fill: 'none',
      stroke: PALETTE[segment.color] || segment.color,
      'stroke-width': thickness,
      'stroke-dasharray': `${length} ${circumference - length}`,
      'stroke-dashoffset': -offset,
      transform: `rotate(-90 ${center} ${center})`,
    });

    const title = svgEl('title');
    title.textContent = `${segment.label}: ${segment.value}`;
    arc.append(title);
    svg.append(arc);

    offset += length;
  }

  const value = svgEl('text', {
    x: center, y: center - 2, 'text-anchor': 'middle',
    'font-size': '22', 'font-weight': '680', fill: '#101828',
  });
  value.textContent = String(total);
  svg.append(value);

  const caption = svgEl('text', {
    x: center, y: center + 16, 'text-anchor': 'middle',
    'font-size': '11', fill: '#98a2b3',
  });
  caption.textContent = 'барлығы';
  svg.append(caption);

  return svg;
}

function shortDate(iso) {
  if (!iso) return '';
  const [, month, day] = iso.split('-');
  return `${day}.${month}`;
}
