import {http} from '../http.js?v=4';
import {icon} from '../ui/icons.js?v=4';
import {esc, on} from '../ui/dom.js?v=4';
import {toast} from '../ui/toast.js?v=4';

/**
 * Game upload (admin): one or many ROM files, sent one at a time to the server library.
 */

const systemNames = {
    gba: 'Game Boy Advance', pcsx: 'PlayStation', mame: 'Arcade (MAME/FBNeo)', nes: 'NES',
    snes: 'Super Nintendo', n64: 'Nintendo 64', dos: 'DOS',
};

let root = null;
let cfg = null; // {systems, companions, maxMB}
let items = []; // {id, file, name, status: wait|sending|done|exists|error, pct, msg, system}
let running = false;
let system = ''; // '' = detect by extension
let overwrite = false;
let finished = false;
let seq = 0;
let tried = false;

const label = (id) => systemNames[id] || id.toUpperCase();
const ext = (name) => (name.includes('.') ? name.split('.').pop().toLowerCase() : '');
const size = (n) => (n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(2)} GB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`);
const isGame = (e) => cfg.systems.some((s) => s.exts.includes(e));
const isCompanion = (e) => cfg.companions.includes(e);

const statusBadge = (it) => {
    switch (it.status) {
        case 'done': return '<span class="badge success">Enviado</span>';
        case 'sending': return `<span class="badge cyan">${it.pct}%</span>`;
        case 'exists': return '<span class="badge warning">Já existe</span>';
        case 'error': return '<span class="badge danger">Erro</span>';
        default: return '<span class="badge">Na fila</span>';
    }
};

const row = (it) => `<tr data-row="${it.id}">
    <td class="mono" style="word-break:break-all">${esc(it.name)}${it.msg ? `<div class="muted" style="font-size:12px">${esc(it.msg)}</div>` : ''}</td>
    <td>${esc(it.system ? label(it.system) : 'Automático')}</td>
    <td class="muted" style="white-space:nowrap">${size(it.file.size)}</td>
    <td style="min-width:130px">${it.status === 'sending' ? `<div class="up-bar"><i style="width:${it.pct}%"></i></div>` : ''}${statusBadge(it)}</td>
    <td>${it.status === 'sending' ? '' : `<button class="btn ghost" data-up-remove="${it.id}" aria-label="Remover">${icon('x')}</button>`}</td>
</tr>`;

export const gameUpload = {
    /** Loads the server upload settings (supported systems and size limit). */
    async load() {
        tried = true;
        try {
            cfg = await http.get('/api/admin/games/systems');
        } catch (e) {
            cfg = null;
            toast.error(e.message);
        }
    },

    mount(el) {
        root = el;
        const pick = (e, input) => {
            gameUpload.add([...input.files]);
            input.value = '';
        };
        on(root, 'change', '[data-up-files]', pick);
        on(root, 'change', '[data-up-folder]', pick);
        on(root, 'click', '[data-up-choose]', () => root.querySelector('[data-up-files]')?.click());
        on(root, 'click', '[data-up-choose-folder]', () => root.querySelector('[data-up-folder]')?.click());
        on(root, 'click', '[data-up-start]', () => gameUpload.start());
        on(root, 'click', '[data-up-clear]', () => {
            items = items.filter((i) => i.status === 'sending');
            finished = false;
            gameUpload.update();
        });
        on(root, 'click', '[data-up-remove]', (e, b) => {
            items = items.filter((i) => String(i.id) !== b.dataset.upRemove);
            gameUpload.update();
        });
        on(root, 'click', '[data-up-reload]', () => location.reload());
        on(root, 'change', '[data-up-system]', (e, s) => {
            system = s.value;
            gameUpload.update();
        });
        on(root, 'change', '[data-up-overwrite]', (e, c) => {
            overwrite = c.checked;
        });
        on(root, 'dragover', '[data-up-drop]', (e, z) => {
            e.preventDefault();
            z.classList.add('over');
        });
        on(root, 'dragleave', '[data-up-drop]', (e, z) => z.classList.remove('over'));
        on(root, 'drop', '[data-up-drop]', (e, z) => {
            e.preventDefault();
            z.classList.remove('over');
            gameUpload.add([...(e.dataTransfer?.files || [])]);
        });
    },

    /** Adds files to the queue, skipping unsupported formats and duplicates. */
    add(files) {
        if (!cfg) return;
        const limit = cfg.maxMB * (1 << 20);
        let skipped = 0;
        for (const file of files) {
            const e = ext(file.name);
            if (!isGame(e) && !isCompanion(e)) {
                skipped++;
                continue;
            }
            if (items.some((i) => i.name === file.name && i.file.size === file.size && i.status !== 'done')) continue;
            const it = {id: ++seq, file, name: file.name, status: 'wait', pct: 0, msg: '', system};
            if (file.size > limit) {
                it.status = 'error';
                it.msg = `Maior que o limite de ${cfg.maxMB} MB.`;
            } else if (file.size === 0) {
                it.status = 'error';
                it.msg = 'O arquivo está vazio.';
            }
            items.push(it);
        }
        if (skipped) toast.error(`${skipped} arquivo(s) ignorado(s): formato não suportado.`);
        finished = false;
        gameUpload.update();
    },

    sendOne(it) {
        return new Promise((resolve) => {
            const q = new URLSearchParams({name: it.name});
            if (it.system) q.set('system', it.system);
            if (overwrite) q.set('overwrite', '1');
            const xhr = new XMLHttpRequest();
            xhr.open('PUT', `/api/admin/games?${q}`);
            xhr.withCredentials = true;
            xhr.setRequestHeader('Content-Type', 'application/octet-stream');
            xhr.upload.onprogress = (ev) => {
                if (!ev.lengthComputable) return;
                const pct = Math.min(99, Math.floor((ev.loaded / ev.total) * 100));
                if (pct !== it.pct) {
                    it.pct = pct;
                    gameUpload.updateRow(it);
                }
            };
            xhr.onerror = () => {
                it.status = 'error';
                it.msg = 'Sem conexão com o servidor.';
                resolve();
            };
            xhr.onload = () => {
                let data = null;
                try {
                    data = JSON.parse(xhr.responseText);
                } catch { /* empty body */ }
                if (xhr.status >= 200 && xhr.status < 300) {
                    it.status = 'done';
                    it.pct = 100;
                    it.msg = '';
                } else {
                    const err = data?.error || {};
                    it.status = err.code === 'exists' ? 'exists' : 'error';
                    it.msg = err.code === 'exists' ? 'Marque "Substituir arquivos existentes" para enviar de novo.' : (err.message || 'Falha no envio.');
                }
                resolve();
            };
            xhr.send(it.file);
        });
    },

    async start() {
        if (running || !items.some((i) => i.status === 'wait' || i.status === 'exists')) return;
        running = true;
        finished = false;
        for (const it of items) {
            if (it.status === 'exists') {
                it.status = 'wait';
                it.msg = '';
            }
        }
        gameUpload.update();
        for (const it of items.filter((i) => i.status === 'wait')) {
            it.status = 'sending';
            it.pct = 0;
            gameUpload.update();
            await gameUpload.sendOne(it);
            gameUpload.update();
        }
        running = false;
        const sent = items.filter((i) => i.status === 'done');
        if (sent.length) {
            try {
                await http.post('/api/admin/games/rescan');
                finished = true;
                toast.success(`${sent.length} arquivo(s) enviado(s). A biblioteca está sendo atualizada.`);
            } catch (e) {
                toast.error(e.message);
            }
        }
        gameUpload.update();
    },

    updateRow(it) {
        const tr = root?.querySelector(`[data-row="${it.id}"]`);
        if (tr) tr.outerHTML = row(it);
    },

    update() {
        const el = root?.querySelector('[data-games]');
        if (el) el.innerHTML = gameUpload.render();
    },

    render() {
        if (!cfg && !tried) return '<p class="muted">Carregando…</p>';
        if (!cfg) {
            return `<div class="card empty">${icon('upload', 'xl')}<h3>Envio de jogos indisponível</h3>
                <p>O envio está desativado na configuração ou o servidor não respondeu.</p></div>`;
        }
        const waiting = items.filter((i) => i.status === 'wait' || i.status === 'exists').length;
        const accept = [...new Set([...cfg.systems.flatMap((s) => s.exts), ...cfg.companions])].map((e) => `.${e}`).join(',');
        return `<div class="card up-card">
            <div class="up-drop" data-up-drop>
                ${icon('upload', 'xl')}
                <b>Arraste os jogos aqui</b>
                <span class="muted">ou escolha um ou vários arquivos. Máximo de ${cfg.maxMB} MB por arquivo.</span>
                <div class="up-actions">
                    <button class="btn primary" data-up-choose ${running ? 'disabled' : ''}>${icon('upload')}Escolher arquivos</button>
                    <button class="btn outline" data-up-choose-folder ${running ? 'disabled' : ''}>${icon('library')}Escolher pasta</button>
                </div>
                <input type="file" multiple hidden data-up-files accept="${esc(accept)}"/>
                <input type="file" multiple hidden webkitdirectory data-up-folder/>
            </div>
            <div class="up-opts">
                <label>Sistema
                    <select data-up-system ${running ? 'disabled' : ''}>
                        <option value="" ${system === '' ? 'selected' : ''}>Automático (pela extensão)</option>
                        ${cfg.systems.map((s) => `<option value="${esc(s.id)}" ${system === s.id ? 'selected' : ''}>${esc(label(s.id))}</option>`).join('')}
                    </select>
                </label>
                <label class="up-check"><input type="checkbox" data-up-overwrite ${overwrite ? 'checked' : ''}/> Substituir arquivos existentes</label>
            </div>
            <p class="muted" style="font-size:13px">Formatos: ${cfg.systems.map((s) => `${esc(label(s.id))} (${s.exts.map((e) => '.' + esc(e)).join(' ')})`).join(' · ')}.
            Jogos de disco com <b>.cue</b> precisam também dos arquivos <b>.bin</b>: escolha o sistema e envie todos juntos.
            Arquivos <b>.zip</b> servem para Arcade e DOS: escolha o sistema.</p>
        </div>
        ${items.length ? `<div class="table-wrap"><table class="table"><thead><tr><th>Arquivo</th><th>Sistema</th><th>Tamanho</th><th>Situação</th><th></th></tr></thead>
            <tbody>${items.map(row).join('')}</tbody></table></div>
            <div class="up-actions">
                <button class="btn primary" data-up-start ${running || !waiting ? 'disabled' : ''}>${icon('upload')}${running ? 'Enviando…' : `Enviar ${waiting} arquivo(s)`}</button>
                <button class="btn outline" data-up-clear ${running ? 'disabled' : ''}>Limpar lista</button>
            </div>` : ''}
        ${finished ? `<div class="card" style="display:flex;align-items:center;gap:16px;padding:16px">${icon('check', 'lg')}
            <div style="flex:1"><b>Envio concluído</b><p class="muted" style="font-size:13px">Os servidores estão atualizando a biblioteca e buscando capas e dados dos novos jogos. Recarregue a página para vê-los.</p></div>
            <button class="btn outline" data-up-reload>${icon('refresh')}Recarregar</button></div>` : ''}`;
    },
};
