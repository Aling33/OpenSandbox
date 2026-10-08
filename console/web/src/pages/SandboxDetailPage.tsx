// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { Alert, Button, Card, Descriptions, InputNumber, Modal, Space, Typography, message } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';

import { sandboxApi } from '../api/client';
import type { Sandbox } from '../api/types';
import { sandboxDisplayName } from '../utils/format';

export function SandboxDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [sandbox, setSandbox] = useState<Sandbox | null>(null);
  const [loading, setLoading] = useState(true);
  const [renewOpen, setRenewOpen] = useState(false);
  const [renewHours, setRenewHours] = useState(1);
  const [endpointPort, setEndpointPort] = useState<number>(8080);
  const [endpointInfo, setEndpointInfo] = useState<Record<string, unknown> | null>(null);
  const [actionLoading, setActionLoading] = useState(false);

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      setSandbox(await sandboxApi.get(id));
    } catch (e) {
      message.error(e instanceof Error ? e.message : '加载失败');
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  if (!id) return null;
  if (loading && !sandbox) return <Typography.Text>加载中…</Typography.Text>;
  if (!sandbox) return <Alert type="error" message="未找到沙箱" />;

  const state = sandbox.status?.state ?? 'Unknown';

  return (
    <div>
      <Typography.Title level={4}>{sandboxDisplayName(sandbox)}</Typography.Title>
      <Card>
        <Descriptions column={1} bordered size="small">
          <Descriptions.Item label="ID">{sandbox.id}</Descriptions.Item>
          <Descriptions.Item label="状态">{state}</Descriptions.Item>
          <Descriptions.Item label="镜像">{sandbox.image?.uri ?? '—'}</Descriptions.Item>
        </Descriptions>
        <Space style={{ marginTop: 16 }}>
          <Button danger loading={actionLoading} onClick={() => void sandboxApi.remove(id).then(() => navigate('/sandboxes'))}>
            删除
          </Button>
          <Button onClick={() => setRenewOpen(true)}>续期</Button>
          <Button
            onClick={() =>
              void sandboxApi.endpoint(id, endpointPort).then(setEndpointInfo)
            }
          >
            获取 Endpoint
          </Button>
          <InputNumber min={1} max={65535} value={endpointPort} onChange={(v) => setEndpointPort(Number(v) || 8080)} />
        </Space>
        {endpointInfo && (
          <pre style={{ marginTop: 16 }}>{JSON.stringify(endpointInfo, null, 2)}</pre>
        )}
      </Card>
      <Modal open={renewOpen} title="续期" onCancel={() => setRenewOpen(false)} onOk={async () => {
        setActionLoading(true);
        try {
          const expiresAt = new Date(Date.now() + renewHours * 3600_000).toISOString();
          await sandboxApi.renewExpiration(id, expiresAt);
          setRenewOpen(false);
          await load();
        } finally {
          setActionLoading(false);
        }
      }}>
        <InputNumber min={1} value={renewHours} onChange={(v) => setRenewHours(Number(v) || 1)} addonAfter="小时" />
      </Modal>
    </div>
  );
}
