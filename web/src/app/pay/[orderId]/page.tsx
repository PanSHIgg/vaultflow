import { Cashier } from "./cashier";

export default async function PayPage({ params }: { params: Promise<{ orderId: string }> }) {
  const { orderId } = await params;
  return <Cashier orderId={orderId} />;
}
